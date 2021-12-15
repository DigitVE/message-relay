package main

import (
	"context"
	"fmt"
	"github.com/jackc/pgconn"
	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgproto3/v2"
	"github.com/joho/godotenv"
	"github.com/segmentio/kafka-go"
	"log"
	"os"
	"strconv"
	"time"
)

func init() {
	if err := godotenv.Load(); err != nil {
		log.Print("No .env file found")
	}
}

func main() {
	listenedTable, _ := os.LookupEnv("RELAY_LISTENED_TABLE")
	outputPlugin, _ := os.LookupEnv("RELAY_OUTPUT_PLUGIN")
	pgConnection, _ := os.LookupEnv("RELAY_PG_CONNECTION")
	conn, err := pgconn.Connect(context.Background(), pgConnection)
	if err != nil {
		log.Fatalln("failed to connect to PostgreSQL server:", err)
	}
	defer conn.Close(context.Background())

	result := conn.Exec(context.Background(), "DROP PUBLICATION IF EXISTS relay_pub;")
	_, err = result.ReadAll()
	if err != nil {
		log.Fatalln("drop publication if exists error", err)
	}

	result = conn.Exec(context.Background(), fmt.Sprintf("CREATE PUBLICATION relay_pub FOR TABLE %s;", listenedTable))
	_, err = result.ReadAll()
	if err != nil {
		log.Fatalln("create publication error", err)
	}
	log.Println("create publication relay_pub")

	var pluginArguments []string
	if outputPlugin == "pgoutput" {
		pluginArguments = []string{"proto_version '1'", "publication_names 'relay_pub'"}
	} else if outputPlugin == "wal2json" {
		pluginArguments = []string{"\"pretty-print\" 'true'"}
	}

	sysident, err := pglogrepl.IdentifySystem(context.Background(), conn)
	if err != nil {
		log.Fatalln("IdentifySystem failed:", err)
	}
	log.Println("SystemID:", sysident.SystemID, "Timeline:", sysident.Timeline, "XLogPos:", sysident.XLogPos, "DBName:", sysident.DBName)

	slotName := "relay_slot"

	_, err = pglogrepl.CreateReplicationSlot(context.Background(), conn, slotName, outputPlugin, pglogrepl.CreateReplicationSlotOptions{Temporary: true})
	if err != nil {
		log.Fatalln("CreateReplicationSlot failed:", err)
	}
	log.Println("Created temporary replication slot:", slotName)

	err = pglogrepl.StartReplication(context.Background(), conn, slotName, sysident.XLogPos, pglogrepl.StartReplicationOptions{PluginArgs: pluginArguments})
	if err != nil {
		log.Fatalln("StartReplication failed:", err)
	}
	log.Println("Logical replication started on slot", slotName)

	clientXLogPos := sysident.XLogPos
	standbyMessageTimeout := time.Second * 10
	nextStandbyMessageDeadline := time.Now().Add(standbyMessageTimeout)

	for {
		if time.Now().After(nextStandbyMessageDeadline) {
			err = pglogrepl.SendStandbyStatusUpdate(context.Background(), conn, pglogrepl.StandbyStatusUpdate{WALWritePosition: clientXLogPos})
			if err != nil {
				log.Fatalln("SendStandbyStatusUpdate failed:", err)
			}

			nextStandbyMessageDeadline = time.Now().Add(standbyMessageTimeout)
		}

		ctx, cancel := context.WithDeadline(context.Background(), nextStandbyMessageDeadline)
		msg, err := conn.ReceiveMessage(ctx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) {
				continue
			}
			log.Fatalln("ReceiveMessage failed:", err)
		}

		switch msg := msg.(type) {
		case *pgproto3.CopyData:
			switch msg.Data[0] {
			case pglogrepl.XLogDataByteID:
				xld, err := pglogrepl.ParseXLogData(msg.Data[1:])
				if err != nil {
					log.Fatalln("ParseXLogData failed:", err)
				}
				if xld.WALData[0] == 'I' {
					log.Println("XLogData =>", "WALStart", xld.WALStart, "ServerWALEnd", xld.ServerWALEnd, "ServerTime:", xld.ServerTime, "WALData", string(xld.WALData))
					logicalMsg, err := pglogrepl.Parse(xld.WALData)
					if err != nil {
						log.Fatalf("Parse logical replication message: %s", err)
					}
					relationID := int32(xld.WALData[1])
					log.Printf("Receive a logical replication message: %s %d", logicalMsg.Type(), relationID)

					clientXLogPos = xld.WALStart + pglogrepl.LSN(len(xld.WALData))

					dataToProduce := getDataFromWALEntryByColumnNumber(xld.WALData[4:])

					produceToKafka(string(dataToProduce))
				}
			}
		default:
			log.Printf("Received unexpected message: %#v\n", msg)
		}
	}
}

func getDataFromWALEntryByColumnNumber(WALData []byte) []byte {
	// Set of 4 bytes (116, 0, 0, 0) means separator

	neededColumnNumber, _ := os.LookupEnv("RELAY_TABLE_COLUMN_NUMBER")
	convertedNeededColumnNumber, _ := strconv.Atoi(neededColumnNumber)

	isStartedSeparator := false
	zeroElementsCount := 0
	columnsCount := 0

	dataToProduce := make([]byte, 0) // Slice with undefined length may impact performance. Possible refactoring?

	for _, element := range WALData {
		if element == 116 {
			isStartedSeparator = true
		} else if element == 0 && isStartedSeparator == true && zeroElementsCount == 2 {
			columnsCount++

			isStartedSeparator = false
			zeroElementsCount = 0
		} else if element == 0 && isStartedSeparator == true {
			zeroElementsCount++
		} else {
			isStartedSeparator = false
			zeroElementsCount = 0
		}

		if columnsCount == convertedNeededColumnNumber {
			dataToProduce = append(dataToProduce, element)
		}
	}

	return dataToProduce
}

func produceToKafka(data string) {
	topic, _ := os.LookupEnv("RELAY_KAFKA_TOPIC")
	host, _ := os.LookupEnv("RELAY_KAFKA_HOST")
	partition, _ := os.LookupEnv("RELAY_KAFKA_PARTITION")
	convertedPartition, _ := strconv.Atoi(partition)

	log.Printf("Trying connect to: %s\n", host)

	conn, err := kafka.DialLeader(context.Background(), "tcp", host, topic, convertedPartition)
	if err != nil {
		log.Fatal("failed to dial leader:", err)
	}

	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = conn.WriteMessages(
		kafka.Message{Value: []byte(data)},
	)
	if err != nil {
		log.Fatal("failed to write messages:", err)
	}

	if err := conn.Close(); err != nil {
		log.Fatal("failed to close writer:", err)
	}

	log.Printf("Produced message to kafka successfully: %s\n", data)
}

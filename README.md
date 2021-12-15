# Message Relay

Message relay written in golang for PostgreSQL and Apache Kafka

## Requirements

Docker and Docker Compose

## Local installation and using

```
docker-compose up -d
docker exec -it message-relay-postgres_1 /bin/bash
psql -U replication_test
CREATE TABLE testovich (data varchar(50));
INSERT INTO testovich VALUES ('happy data garbage!');
```

## How to check message relay log

```
docker logs message-relay_1
```

## TODO

* Skipped messages issue while relay inactive. Any ideas about that? Connection settings? (Try to keep publications and slots)
* Test wal interpreting messages with other than 8 bit charsets
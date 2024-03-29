package main

import (
	"context"
	"encoding/hex"
	"log"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	PGLOGREPL_DEMO_CONN_STRING = "postgres://user:password@127.0.0.1:5434/mydatabase?replication=database"
)

const (
	OutputPluginPGOutput = "pgoutput"
	OutputPluginWal2JSON = "wal2json"
)

func main() {
	const outputPlugin = OutputPluginPGOutput // Change this to use a different output plugin if needed

	conn, err := pgconn.Connect(context.Background(), PGLOGREPL_DEMO_CONN_STRING)
	if err != nil {
		log.Fatalln("failed to connect to PostgreSQL server:", err)
	}
	defer conn.Close(context.Background())

	if err := dropAndCreatePublication(conn); err != nil {
		log.Fatalln("failed to drop or create publication:", err)
	}

	pluginArguments := getPluginArguments(outputPlugin)

	sysident, err := pglogrepl.IdentifySystem(context.Background(), conn)
	if err != nil {
		log.Fatalln("IdentifySystem failed:", err)
	}
	log.Println("SystemID:", sysident.SystemID, "Timeline:", sysident.Timeline, "XLogPos:", sysident.XLogPos, "DBName:", sysident.DBName)

	slotName := "pglogrepl_demo"
	if err := createReplicationSlot(conn, slotName, outputPlugin); err != nil {
		log.Fatalln("CreateReplicationSlot failed:", err)
	}
	defer func() {
		if err := conn.Exec(context.Background(), "DROP_REPLICATION_SLOT "+slotName).Close(context.Background()); err != nil {
			log.Println("failed to drop replication slot:", err)
		}
		log.Println("Dropped replication slot:", slotName)
	}()

	if err := startReplication(conn, slotName, sysident.XLogPos, pluginArguments); err != nil {
		log.Fatalln("StartReplication failed:", err)
	}
	log.Println("Logical replication started on slot", slotName)

	clientXLogPos := sysident.XLogPos
	standbyMessageTimeout := time.Second * 10
	nextStandbyMessageDeadline := time.Now().Add(standbyMessageTimeout)
	relations := map[uint32]*pglogrepl.RelationMessage{}
	typeMap := pgtype.NewMap()

	for {
		if time.Now().After(nextStandbyMessageDeadline) {
			if err := sendStandbyStatusUpdate(conn, clientXLogPos); err != nil {
				log.Fatalln("SendStandbyStatusUpdate failed:", err)
			}
			log.Println("Sent Standby status message")
			nextStandbyMessageDeadline = time.Now().Add(standbyMessageTimeout)
		}

		ctx, cancel := context.WithDeadline(context.Background(), nextStandbyMessageDeadline)
		rawMsg, err := conn.ReceiveMessage(ctx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) {
				continue
			}
			log.Fatalln("ReceiveMessage failed:", err)
		}

		if errMsg, ok := rawMsg.(*pgproto3.ErrorResponse); ok {
			log.Fatalf("received Postgres WAL error: %+v", errMsg)
		}

		msg, ok := rawMsg.(*pgproto3.CopyData)
		if !ok {
			log.Printf("Received unexpected message: %T\n", rawMsg)
			continue
		}

		switch msg.Data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			pkm, err := pglogrepl.ParsePrimaryKeepaliveMessage(msg.Data[1:])
			if err != nil {
				log.Fatalln("ParsePrimaryKeepaliveMessage failed:", err)
			}
			log.Println("Primary Keepalive Message =>", "ServerWALEnd:", pkm.ServerWALEnd, "ServerTime:", pkm.ServerTime, "ReplyRequested:", pkm.ReplyRequested)

			if pkm.ReplyRequested {
				nextStandbyMessageDeadline = time.Time{}
			}

		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(msg.Data[1:])
			if err != nil {
				log.Fatalln("ParseXLogData failed:", err)
			}
			log.Printf("XLogData => WALStart %s ServerWALEnd %s ServerTime %s WALData:\n%s\n", xld.WALStart, xld.ServerWALEnd, xld.ServerTime, hex.Dump(xld.WALData))
			logicalMsg, err := pglogrepl.Parse(xld.WALData)
			if err != nil {
				log.Fatalf("Parse logical replication message: %s", err)
			}
			log.Printf("Receive a logical replication message: %s", logicalMsg.Type())
			switch logicalMsg := logicalMsg.(type) {
			case *pglogrepl.RelationMessage:
				relations[logicalMsg.RelationID] = logicalMsg

			case *pglogrepl.BeginMessage:
				// Indicates the beginning of a group of changes in a transaction. This is only sent for committed transactions. You won't get any events from rolled back transactions.

			case *pglogrepl.CommitMessage:

			case *pglogrepl.InsertMessage:
				handleInsertMessage(logicalMsg, relations)

			case *pglogrepl.UpdateMessage:
				// ...
			case *pglogrepl.DeleteMessage:
				// ...
			case *pglogrepl.TruncateMessage:
				// ...

			case *pglogrepl.TypeMessage:
			case *pglogrepl.OriginMessage:
			default:
				log.Printf("Unknown message type in pgoutput stream: %T", logicalMsg)
			}

			clientXLogPos = xld.WALStart + pglogrepl.LSN(len(xld.WALData))
		}
	}
}

package main

import (
	"encoding/binary"
	"os"

	"github.com/RenseiAI/donmai/attachwire"
)

func i64(value int64) *int64       { return &value }
func stringp(value string) *string { return &value }

func writeRecord(raw []byte) {
	if err := binary.Write(os.Stdout, binary.BigEndian, uint64(len(raw))); err != nil {
		panic(err)
	}
	if _, err := os.Stdout.Write(raw); err != nil {
		panic(err)
	}
}

func main() {
	writeRecord([]byte(attachwire.ProtocolVersion))
	writeRecord([]byte(attachwire.SubprotocolVersion))
	writeRecord([]byte(attachwire.VersionPathSegment))

	for eventType := attachwire.TypeOutput; eventType <= attachwire.TypeControl; eventType++ {
		writeRecord((attachwire.Frame{
			Type: eventType, Seq: 18446744073709551615, RelTime: 16384,
			Payload: []byte{0x00, 0x0a, 0x0d, 0x7f, 0x80, 0xff},
		}).Encode())
	}

	messages := []attachwire.ControlMessage{
		attachwire.Subscribe{SessionID: "session", AsRole: attachwire.RoleHost, Epoch: i64(7), ResumeFrom: i64(8), ResumeEpoch: i64(9), Viewport: &attachwire.Viewport{Cols: 80, Rows: 24}},
		attachwire.ResumeFrom{Seq: 10, Epoch: i64(7)},
		attachwire.SnapshotRequest{Reason: attachwire.ReasonResync},
		attachwire.Kill{Reason: attachwire.KillStopped, Signal: stringp("SIGTERM")},
		attachwire.Grab{},
		attachwire.Release{},
		attachwire.Presence{Op: attachwire.PresenceList, Members: []attachwire.PresenceMember{{UserID: "user", ConnID: "conn", Role: "driver", Driving: true}}},
		attachwire.InputAck{AckInputSeq: 11},
		attachwire.PenGranted{UserID: "user", ConnID: "conn", PenGeneration: 12},
		attachwire.PenRevoked{UserID: "user", ConnID: "conn", PenGeneration: 13},
		attachwire.PenState{HolderUserID: stringp("user"), HolderConnID: stringp("conn"), PenGeneration: 14},
		attachwire.RoomState{State: attachwire.RoomLive, SinceSeq: i64(15)},
		attachwire.ControlError{Code: attachwire.CodeFraming, Message: "bad", Retryable: false},
	}
	for _, message := range messages {
		raw, err := attachwire.MarshalControl(message)
		if err != nil {
			panic(err)
		}
		writeRecord(raw)
	}
}

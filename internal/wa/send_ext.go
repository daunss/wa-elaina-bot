package wa

import (
	"context"
	"io"
	"net/http"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	pbf "google.golang.org/protobuf/proto"
)

func TextMsg(s string) *waProto.Message {
	return &waProto.Message{ Conversation: pbf.String(s) }
}

func TextMentionMsg(text string, jids []types.JID) *waProto.Message {
	ci := &waProto.ContextInfo{}
	for _, j := range jids { ci.MentionedJID = append(ci.MentionedJID, j.String()) }
	return &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text: pbf.String(text), ContextInfo: ci,
		},
	}
}

func SendImageURL(client *whatsmeow.Client, to types.JID, url, caption string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := http.Get(url)
	if err != nil { return err }
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	msg := &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			Caption: pbf.String(caption),
		},
	}
	// upload via whatsmeow helper
	up, err := client.Upload(ctx, data, whatsmeow.MediaImage)
	if err != nil { return err }
	msg.ImageMessage.URL = pbf.String(up.URL)
	msg.ImageMessage.DirectPath = pbf.String(up.DirectPath)
	msg.ImageMessage.MediaKey = up.MediaKey
	msg.ImageMessage.FileEncSHA256 = up.FileEncSHA256
	msg.ImageMessage.FileSHA256 = up.FileSHA256
	msg.ImageMessage.FileLength = pbf.Uint64(uint64(len(data)))

	_, err = client.SendMessage(ctx, to, msg)
	return err
}

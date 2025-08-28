package wa

import (
	"context"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
)

type Sender struct{ C *whatsmeow.Client }

func NewSender(c *whatsmeow.Client) *Sender { return &Sender{C: c} }

func DestJID(j types.JID) types.JID {
	if j.Server == types.GroupServer {
		return j
	}
	return j.ToNonAD()
}

func (s *Sender) retry(m *waProto.Message, to types.JID, err error) error {
	if err != nil && strings.Contains(err.Error(), "479") {
		time.Sleep(2 * time.Second)
		_, err = s.C.SendMessage(context.Background(), to, m)
	}
	return err
}

func (s *Sender) Text(to types.JID, text string) error {
	msg := &waProto.Message{Conversation: proto.String(text)}
	_, err := s.C.SendMessage(context.Background(), to, msg)
	return s.retry(msg, to, err)
}

func (s *Sender) Audio(to types.JID, audio []byte, mime string, ptt bool, seconds uint32) error {
	up, err := s.C.Upload(context.Background(), audio, whatsmeow.MediaAudio)
	if err != nil { return err }
	msg := &waProto.Message{
		AudioMessage: &waProto.AudioMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(audio))),
			Mimetype:      proto.String(mime),
			PTT:           proto.Bool(ptt),
			Seconds:       proto.Uint32(seconds),
		},
	}
	_, err = s.C.SendMessage(context.Background(), to, msg)
	return s.retry(msg, to, err)
}

func (s *Sender) Video(to types.JID, video []byte, mime, caption string) error {
	up, err := s.C.Upload(context.Background(), video, whatsmeow.MediaVideo)
	if err != nil { return err }
	msg := &waProto.Message{
		VideoMessage: &waProto.VideoMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(video))),
			Mimetype:      proto.String(mime),
			Caption:       proto.String(strings.TrimSpace(caption)),
		},
	}
	_, err = s.C.SendMessage(context.Background(), to, msg)
	return s.retry(msg, to, err)
}

func (s *Sender) Image(to types.JID, image []byte, mime, caption string) error {
	up, err := s.C.Upload(context.Background(), image, whatsmeow.MediaImage)
	if err != nil { return err }
	msg := &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(image))),
			Mimetype:      proto.String(mime),
			Caption:       proto.String(strings.TrimSpace(caption)),
		},
	}
	_, err = s.C.SendMessage(context.Background(), to, msg)
	return s.retry(msg, to, err)
}

func (s *Sender) Document(to types.JID, data []byte, mime, filename, caption string) error {
	up, err := s.C.Upload(context.Background(), data, whatsmeow.MediaDocument)
	if err != nil { return err }
	if filename == "" { filename = "file" }
	if mime == "" { mime = "application/octet-stream" }
	msg := &waProto.Message{
		DocumentMessage: &waProto.DocumentMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
			Mimetype:      proto.String(mime),
			Title:         proto.String(filename),
			FileName:      proto.String(filename),
			Caption:       proto.String(strings.TrimSpace(caption)),
		},
	}
	_, err = s.C.SendMessage(context.Background(), to, msg)
	return s.retry(msg, to, err)
}
package wa

import (
  "context"; "strings"; "time"
  "google.golang.org/protobuf/proto"
  "go.mau.fi/whatsmeow"
  waProto "go.mau.fi/whatsmeow/binary/proto"
  "go.mau.fi/whatsmeow/types"
)

type Sender struct{ C *whatsmeow.Client }

func DestJID(j types.JID) types.JID { if j.Server==types.GroupServer { return j }; return j.ToNonAD() }

func (s *Sender) retrySend(m *waProto.Message, to types.JID, err error) error {
  if err != nil && strings.Contains(err.Error(), "479") {
    time.Sleep(2*time.Second)
    _, err = s.C.SendMessage(context.Background(), to, m)
  }
  return err
}

func (s *Sender) Text(to types.JID, text string) error {
  m := &waProto.Message{Conversation: proto.String(text)}
  _, err := s.C.SendMessage(context.Background(), to, m)
  return s.retrySend(m, to, err)
}
func (s *Sender) Audio(to types.JID, bytes []byte, mime string, ptt bool, seconds uint32) error { /* pindahkan isi sendAudio sekarang */ return nil }
func (s *Sender) Video(to types.JID, bytes []byte, mime, caption string) error { /* pindahkan isi sendVideo */ return nil }
func (s *Sender) Image(to types.JID, bytes []byte, mime, caption string) error { /* pindahkan isi sendImage */ return nil }
func (s *Sender) Document(to types.JID, bytes []byte, mime, filename, caption string) error { /* pindahkan isi sendDocument */ return nil }

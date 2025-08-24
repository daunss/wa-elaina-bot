package ba
import ("context"; "sync"; "wa-elaina/anime"; "go.mau.fi/whatsmeow"; "go.mau.fi/whatsmeow/types")

type Manager struct{ once sync.Once; links []string; url, local string }

func New(url, local string) *Manager { return &Manager{url:url, local:local} }

func (m *Manager) ensure(ctx context.Context) error {
  var err error
  m.once.Do(func() {
    m.links, err = anime.LoadLinks(ctx, m.local, m.url)
  })
  return err
}

func (m *Manager) MaybeHandle(ctx context.Context, client *whatsmeow.Client, chat types.JID, text string) bool {
  if !anime.IsBARequest(text) { return false }
  if err := m.ensure(ctx); err != nil { return false }
  _ = anime.SendRandomImage(ctx, client, chat, m.links)
  return true
}

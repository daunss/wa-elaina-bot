package ba

import (
	"context"
	"sync"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	anime "wa-elaina/anime"
)

type Manager struct {
	once  sync.Once
	links []string
	URL   string
	Local string
	err   error
}

func New(url, local string) *Manager { return &Manager{URL: url, Local: local} }

func (m *Manager) ensure(ctx context.Context) error {
	m.once.Do(func() {
		m.links, m.err = anime.LoadLinks(ctx, m.Local, m.URL)
	})
	return m.err
}

func (m *Manager) MaybeHandle(ctx context.Context, client *whatsmeow.Client, chat types.JID, text string) bool {
	if !anime.IsBARequest(text) {
		return false
	}
	if err := m.ensure(ctx); err != nil {
		return false
	}
	_ = anime.SendRandomImage(ctx, client, chat, m.links)
	return true
}

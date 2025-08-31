package main

import (
	"context"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/joho/godotenv"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	_ "modernc.org/sqlite"

	"wa-elaina/internal/bot"
	"wa-elaina/internal/config"
	"wa-elaina/internal/httpapi"
	"wa-elaina/internal/wa"

	// Welcome handler
	wel "wa-elaina/internal/feature/welcome"
)

var waReady atomic.Bool

func main() {
	_ = godotenv.Load()

	cfg := config.Load()
	dbLog := waLog.Stdout("Database", "INFO", true)
	dsn := "file:" + cfg.SessionDB + "?_pragma=foreign_keys(1)"

	container, err := sqlstore.New(context.Background(), "sqlite", dsn, dbLog)
	if err != nil {
		log.Fatal(err)
	}

	device, err := container.GetFirstDevice(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	if device == nil {
		device = container.NewDevice()
	}

	client := whatsmeow.NewClient(device, nil)
	sender := wa.NewSender(client)

	// Router semua fitur (untuk pesan/chat)
	rt := bot.NewRouter(cfg, sender, &waReady)

	// Welcome handler dari ENV
	welH := wel.NewFromEnv()

	client.AddEventHandler(func(e any) {
		switch ev := e.(type) {
		case *events.Connected, *events.AppStateSyncComplete:
			waReady.Store(true)
			log.Println("WhatsApp state: READY (connected & app state synced)")
		case *events.Disconnected:
			waReady.Store(false)
			log.Println("WhatsApp state: DISCONNECTED")

		// Pesan masuk → serahkan ke router
		case *events.Message:
			if waReady.Load() {
				rt.HandleMessage(client, ev)
			}
		}

		// Welcome handler (peserta grup baru)
		_ = welH.TryHandle(client, e)
	})

	log.Printf("Bot %s is running...", cfg.BotName)

	// Connect WA
	if client.Store.ID == nil {
		qr, _ := client.GetQRChannel(context.Background())
		if err := client.Connect(); err != nil {
			log.Fatal(err)
		}
		for e := range qr {
			if e.Event == "code" {
				log.Println("Scan QR (code):", e.Code)
			}
			if e.Event == "success" {
				log.Println("Login success")
			}
		}
	} else if err := client.Connect(); err != nil {
		log.Fatal(err)
	}

	// HTTP API
	api := httpapi.New(cfg, sender, &waReady)
	api.RegisterHandlers(http.DefaultServeMux)

	log.Printf("Mode: %s | Trigger: %q | HTTP :%s", cfg.Mode, cfg.Trigger, cfg.Port)
	srv := &http.Server{Addr: ":" + cfg.Port, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

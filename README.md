# wa-elaina-bot (Elaina — WhatsApp Assistant)

Asisten WhatsApp berkarakter **Elaina**: ngobrol natural (Gemini), jawab **gambar** (Vision), transkrip **voice note (VN)** → **auto‑reply** saat menyebut *Elaina*, serta **downloader TikTok (TikWM only)** lengkap dengan **link audio** dan dukungan **slide**. Tersedia **HTTP API** kecil untuk mengirim pesan dari aplikasi lain dan siap dijalankan di **Pterodactyl**.

---

## ✨ Fitur Utama

* **Chat AI berkarakter “Elaina”**

  * Persona santai-sopan; bisa dipanggil di grup via trigger (mode MANUAL/AUTO).
* **Vision — Jawab Gambar**

  * Kirim gambar → dianalisis (Gemini 1.5) + jawab singkat/insight.
* **VN → Teks → Auto-reply**

  * VN ditranskrip. Bot **hanya membalas** jika transkrip **menyebut “Elaina”** (fuzzy: *elaina/eleina/elena/elina*).
  * (Opsional debug) kirim transkrip saat tidak ada sebutan.
* **TikTok (TikWM only)**

  * Kirim link TikTok → bot mengirim **video langsung** + **tautan audio**.
  * TikTok **slide** dikirim sebagai **gambar** berurutan.
  * Batas ukuran dapat dikonfigurasi; fallback ke **dokumen**/tautan jika melebihi.
* **Blue Archive (opsional)**

  * Respons fun berbasis *BA* (link/konten yang dikurasi), dapat dimatikan jika tidak perlu.
* **HTTP API**

  * `/send` (dengan API key & rate limit), `/healthz`, `/help`.

> Model default: **Gemini 1.5 Flash** untuk teks/vision/transcribe (Google Generative Language API).

---

## 🧩 Arsitektur Singkat

* `main.go` — wiring WA client, router pesan, persona, handler Vision/VN, Gemini calls.
* `internal/wa/` — util pengiriman (text, audio, gambar, dokumen) via whatsmeow.
* `internal/tiktok/` — handler TikTok (TikWM only): unduh, cek ukuran, kirim media/slide, sertakan link audio.
* `internal/httpapi/` — HTTP server kecil: help/healthz/send + rate limiting.
* `internal/config/` — loader konfigurasi `.env`/ENV.
* `internal/ba/` — konten/tautan Blue Archive (opsional).

DB sesi WhatsApp: **SQLite** (pure Go driver `modernc.org/sqlite`) — cukup satu file `session.db`.

---

## ⚙️ Prasyarat

* **Go 1.22+**
* **Akun Google Gemini** (API key)
* (Opsional) **ElevenLabs** API key untuk TTS balasan VN

---

## 🚀 Quickstart (Local)

1. **Clone & install deps**

   ```bash
   git clone <repo-url>
   cd wa-elaina-bot
   go mod download
   ```
2. **Siapkan `.env`** (contoh di bawah). Minimal isi `GEMINI_API_KEYS`.
3. **Build & Run**

   ```bash
   go build -o app .
   ./app
   ```
4. **Login WhatsApp**

   * Saat pertama run, **QR Code** muncul di console. Scan dari HP.
   * Sesi tersimpan di `SESSION_PATH` (default `session.db`).

### Contoh `.env`

```env
# Mode bot
MODE=MANUAL                 # MANUAL: perlu sebutan/trigger di grup, AUTO: selalu balas
TRIGGER=elaina              # Kata panggil di grup
BOT_NAME=Elaina

# WhatsApp session
SESSION_PATH=./session.db

# HTTP server
PORT=7860
SEND_API_KEY=ubah-ini       # kosongkan untuk menonaktifkan /send
SEND_RATE_PER_MIN=10        # rate limit per IP (untuk /send)

# Gemini (pisahkan dengan koma bila lebih dari 1)
GEMINI_API_KEYS=key1,key2

# ElevenLabs (opsional untuk balasan VN sebagai TTS)
ELEVENLABS_API_KEY=
ELEVEN_VOICE=
ELEVEN_MIME=audio/ogg;codecs=opus

# TikTok limits (Byte)
TIKTOK_MAX_VIDEO_MB=50      # batas praktis; internal dikonversi Byte
TIKTOK_MAX_IMAGE_MB=5
TIKTOK_MAX_DOC_MB=80
TIKTOK_MAX_SLIDES=10

# Blue Archive (opsional)
BA_LINKS_URL=
BA_LINKS_LOCAL=

# Debug
VN_DEBUG_TRANSCRIPT=false   # true = kirim transkrip saat tak ada sebutan “Elaina”
```

> **Tip:** Simpan file sesi (`SESSION_PATH`) di lokasi persisten (Docker/Pterodactyl) agar tidak perlu scan ulang.

---

## 🕹️ Cara Pakai (WhatsApp)

* **Obrolan teks:** kirim pesan ke bot. Di grup (MODE=MANUAL), panggil dengan `elaina`/`@elaina`.
* **Vision:** kirim **gambar** (dengan/ tanpa caption). Bot menjawab deskripsi/insight singkat.
* **VN → Auto‑Reply:** kirim **voice note** sambil menyebut **“Elaina”** di ucapan. Bot transkrip & membalas.
* **TikTok:** kirim **URL TikTok** → bot kirim video + link audio. Jika **slide**, bot kirim sebagai rangkaian **gambar**.
* **Perintah:**

  * `!help` — bantuan singkat
  * `!ping` — konektivitas cepat

> Untuk variasi nama *Elaina* yang sering terjadi di transkrip (eleina/elina/elena), deteksi sudah **fuzzy**.

---

## 🌐 HTTP API

### `GET /healthz`

* Cek status hidup.

### `GET /help`

* Ringkas dokumentasi endpoint.

### `POST /send`

Kirim pesan WA ke JID tertentu dari aplikasi eksternal.

* **Headers:** `X-API-Key: <SEND_API_KEY>` (wajib jika `SEND_API_KEY` diset)
* **Body JSON:**

  ```json
  { "to": "62XXXXXXXXXX@s.whatsapp.net", "text": "Halo dari API" }
  ```
* **Rate limit:** `SEND_RATE_PER_MIN` per IP.

> Catatan: hanya **teks** yang didukung pada endpoint ini (sengaja sederhana). Perlu media? Saran: tambah endpoint terpisah atau gunakan bot chat biasa.

---

## 🐳 Deploy di Pterodactyl

**Image:** `ghcr.io/parkervcp/yolks:golang_1.22`

**Startup Command:**

```bash
bash -lc 'export CGO_ENABLED=0 GOOS=linux GOARCH=amd64; \
  go build -trimpath -ldflags "-s -w" -o app ./ && \
  export PORT="${PORT:-${SERVER_PORT}}"; \
  export SESSION_PATH="${SESSION_PATH:-/home/container/session.db}"; \
  ./app'
```

**Variables yang disarankan:**

* `PORT` → `{{SERVER_PORT}}`
* `SESSION_PATH` → `/home/container/session.db`
* `MODE`, `TRIGGER`, `GEMINI_API_KEYS`, `ELEVENLABS_API_KEY`, `SEND_API_KEY`, `SEND_RATE_PER_MIN`, dst.

**Langkah:**

1. Upload source / git clone ke server.
2. Isi **Variables** seperti di atas.
3. Start → scan QR di Console.
4. Test `GET /healthz` memakai port alokasi panel.

---

## 🔐 Keamanan & Privasi

* **API Key**: lindungi endpoint `/send` dengan `SEND_API_KEY` dan rate limit. Tambah whitelist JID bila perlu.
* **Penyimpanan**: file sesi WA berisi kredensial login — simpan di disk yang aman & persisten.
* **Konten pengguna**: transkrip VN & gambar diproses oleh API pihak ketiga (Gemini). Tampilkan kebijakan privasi jika dipakai publik.

---

## 🧪 Troubleshooting

* **VN tak dibalas**: pastikan ucapan menyebut “Elaina” (variasi *eleina/elena/elina* juga dideteksi). Aktifkan `VN_DEBUG_TRANSCRIPT=true` untuk melihat transkrip.
* **Video terlalu besar**: bot akan fallback ke dokumen/tautan jika melewati batas. Perbesar limit via env `TIKTOK_MAX_*` (hati‑hati kuota).
* **Tidak keluar QR**: cek log panel/console; pastikan binary jalan & port terbuka. Hapus `session.db` (terakhir) bila ingin login ulang.
* **Timeout Gemini/unduh**: koneksi lambat—naikkan timeout (kode sudah disiapkan untuk di‑tweak), atau coba ulang.

---

## 🗺️ Roadmap (opsional)

* **Memory percakapan per‑JID** (konteks follow‑up)
* **Admin controls via chat** (`!mode`, `!vn require_mention`, `!voice`)
* **Notes & Reminder** (`!note`, `!remind`) dengan scheduler ringan
* **Metrics Prometheus** (`/metrics`, `/readyz`)
* **Skill/plugin router** (main.go makin ramping)

---

## 🤝 Kontribusi

PR dan issue welcome. Ikuti gaya kode Go standar (gofmt, golangci‑lint jika tersedia). Tambahkan deskripsi fitur dan perubahan ENV bila ada.

---

## 📄 Lisensi

Tentukan lisensi sesuai preferensi (mis. MIT/Apache‑2.0). Jika private, beri keterangan *All rights reserved*.

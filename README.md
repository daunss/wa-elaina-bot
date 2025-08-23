# WA Elaina Bot (Go + whatsmeow + Gemini) — Starter

Template minimal untuk membuat bot WhatsApp yang menjawab memakai **Gemini API** dengan persona **Elaina**.
Cocok untuk **Hugging Face Spaces (free)** atau **VPS**.

## Fitur
- WhatsApp Web Multi-Device via **whatsmeow**
- Endpoint HTTP: `/healthz` (cek hidup) & `/send?to=62xxx&text=...`
- Panggil **Gemini 1.5 Flash** untuk jawaban cepat
- Persona **Elaina** (bisa diatur via env `BOT_NAME`)

---

## 1) Siapkan API Key Gemini
- Buka Google AI Studio → buat API key → simpan sebagai `GEMINI_API_KEY`.

## 2) Deploy di Hugging Face Spaces (FREE)
1. Buat Space baru → **SDK: Docker**.
2. Upload file-file dari repo ini (minimal `Dockerfile`, `main.go`, `README.md`).
3. Di Settings → **App Port** = `7860`.
4. Tambah **Environment Variables**:
   - `GEMINI_API_KEY`: (wajib)
   - `BOT_NAME`: `Elaina` (opsional)
   - `SESSION_PATH`: `/data/session.db` (opsional, default `session.db` di root)
5. Save → tunggu build → buka **Logs**.
6. **Login sekali**: di log cari `Scan QR (code): <kode>`. Salin kode ke *QR generator* lalu **scan dengan WhatsApp** (ponsel yang akan jadi bot).
7. Setelah `Login success`, bot siap dipakai.
8. Pastikan ada aktivitas harian (<48 jam idle) agar Space free **tidak sleep**.

> Catatan: di free tier, storage tidak persisten resmi. Selama Space tidak di-rebuild/di-restart besar, file `session.db` masih tersimpan.

## 3) Deploy di VPS (opsional)
- Install Go 1.22+. 
- `go build -o app .` lalu jalankan `./app`.
- Untuk service permanen, pakai systemd (contoh ada di bawah).

### Contoh systemd service
```
[Unit]
Description=WA Elaina Bot
After=network.target

[Service]
WorkingDirectory=/opt/wa-elaina
ExecStart=/opt/wa-elaina/app
Environment=GEMINI_API_KEY=YOUR_KEY
Environment=BOT_NAME=Elaina
Restart=always
RestartSec=5
User=www-data

[Install]
WantedBy=multi-user.target
```

## 4) Test cepat
- Health check: buka `https://<host>/healthz` → `ok`
- Kirim manual: `GET /send?to=62xxxxxxx&text=halo`

## 5) Variabel Lingkungan
- `GEMINI_API_KEY` (wajib)
- `BOT_NAME` default `Elaina`
- `SESSION_PATH` default `session.db` (di current dir). Di Spaces disarankan `/data/session.db`.

## 6) Keamanan & Batasan
- Ini menggunakan protokol WhatsApp Web (non-resmi). Risiko ToS/ban: gunakan bijak.
- Filter konten berbahaya sebelum kirim ke Gemini.
- Simpan API key di env, jangan hardcode.

## 7) Troubleshooting
- Tidak muncul QR: cek log; pastikan outbound koneksi bebas (Spaces/VPS).
- Sering logout: periksa apakah Space **sleep** atau **rebuild**; scan ulang.
- Error Gemini 4xx/5xx: cek quota/billing, format payload, atau timeout.

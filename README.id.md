# ZafxyrAgent AI

> **Autonomous AI agent yang hidup di root shell HP Android kamu.**
> Ngobrol pakai bahasa sehari-hari, dan agent benar-benar mengeksekusinya — buka aplikasi, cari file, jalankan perintah root, unduh file, sampai membuat pengingat otomatis.

![Platform](https://img.shields.io/badge/platform-Android%2014%2B-3ddc84)
![Root](https://img.shields.io/badge/root-KernelSU%20%7C%20Magisk%20%7C%20APatch-6b7280)
![Daemon](https://img.shields.io/badge/daemon-Go-00ADD8)
![UI](https://img.shields.io/badge/UI-WebView-6b7280)
![License](https://img.shields.io/badge/license-MIT-blue)

---

## Apa ini?

**ZafxyrAgent AI** adalah sebuah **KernelSU / Magisk / APatch module** yang menjalankan daemon kecil (ditulis dengan Go) sebagai `root` di dalam HP Android. Daemon ini:

- Menyediakan **web chat UI** ringan (WebView) di `http://127.0.0.1:8787`
- Menghubungkan HP ke **LLM** (OpenRouter / OpenAI-compatible / Gemini) via function calling
- Mengeksekusi aksi nyata di HP lewat **tool** — bukan sekadar menjawab teks
- Berjalan otomatis saat boot sebagai **systemless module** dengan notifikasi persisten

Singkatnya: **asisten AI yang bisa mengatur HP kamu sendiri.**

---

## Fitur

- **Multi-provider LLM** — OpenRouter, endpoint OpenAI-compatible, atau Google Gemini
- **Automatic model fallback** — model utama error/kena limit, otomatis coba model cadangan
- **13 tool eksekusi** nyata di perangkat (lihat daftar di bawah)
- **Live streaming UI** — jawaban muncul token-per-token + panel progress tiap aksi (SSE)
- **Sugesti chip ("SARAN")** — saran aksi lanjutan yang bisa langsung ditekan
- **Pengingat otomatis** (`schedule_message`) — mis. "ingetin 10 menit lagi minum obat"
- **Pemilih model** langsung dari UI chat
- **Notifikasi foreground service** — tap untuk buka chat, auto-start saat boot
- **Systemless** — dipasang sebagai module, gampang dibersihkan lewat uninstall

---

## Persyaratan

| Kebutuhan | Detail |
|-----------|--------|
| Android | **14+** (minSdk 34) |
| Root | **KernelSU**, **Magisk**, atau **APatch** |
| API Key | Dari salah satu provider (rekomendasi: [OpenRouter](https://openrouter.ai)) |

---

## Instalasi

1. Unduh file module dari halaman **Releases** (`aiagent_notif_vX.Y.Z.zip`)
2. Buka **KernelSU / Magisk / APatch** → **Modules** → **Install from storage**
3. Pilih file zip tadi, lalu **reboot** HP
4. Setelah menyala, tarik notifikasi **"ZafxyrAgent AI"** → tap untuk membuka chat

Saat instalasi, module otomatis memasang aplikasi pendamping (`com.zafxyra.agent`) beserta izin notifikasinya.

---

## Konfigurasi

Edit file konfigurasi di:

```
/data/adb/aiagent/config.json
```

| Field | Tipe | Keterangan |
|-------|------|------------|
| `provider` | string | `openai` (OpenAI-compatible / OpenRouter) atau `gemini` |
| `api_key` | string | API key provider kamu |
| `base_url` | string | Base URL endpoint, mis. `https://openrouter.ai/api/v1` |
| `model` | string | Model utama |
| `fallback_models` | array | Model cadangan saat model utama gagal |
| `models` | array | Daftar model yang muncul di pemilih model UI |
| `max_tokens` | int | Maksimum token balasan |
| `system` | string | System prompt agent |
| `port` | int | Port daemon (default `8787`) |
| `autoopen` | bool | Buka UI otomatis saat boot |
| `allow_shell` | bool | Izinkan tool `run_shell` (akses root shell) |

Setelah mengubah config, restart daemon atau reboot HP agar berlaku.

---

## Contoh perintah

```
buka WhatsApp
cek sisa storage
list file di Download
buka google.com
unduh file dari url ini ke Download
ingetin aku 10 menit lagi minum obat
bikin file catatan.txt isinya daftar belanja
```

---

## Tool yang tersedia

| Tool | Fungsi |
|------|--------|
| `open_app` | Buka aplikasi yang terinstall |
| `open_path` | Buka file/folder dengan aplikasi bawaan |
| `open_url` | Buka link di browser |
| `web_search` | Cari informasi di internet |
| `run_shell` | Jalankan perintah root shell |
| `write_file` | Tulis / buat file baru |
| `read_file` | Baca isi file teks |
| `list_dir` | Tampilkan isi direktori |
| `make_dir` | Buat folder |
| `move_file` | Pindah / ganti nama file atau folder |
| `delete_path` | Hapus file atau folder |
| `download_url` | Unduh file dari internet lalu simpan ke HP |
| `schedule_message` | Jadwalkan pesan otomatis (reminder) |

---

## HTTP API (lokal)

Daemon listen di `127.0.0.1:8787`:

| Endpoint | Method | Fungsi |
|----------|--------|--------|
| `/` | GET | Web chat UI |
| `/chat` | POST | Kirim pesan ke agent |
| `/events` | GET | Stream SSE (status / token / tool / reply) |
| `/models` | GET | Daftar model yang tersedia |
| `/model` | POST | Set model aktif |
| `/model/random` | POST | Pilih model acak |
| `/history` | GET | Riwayat percakapan |
| `/reset` | POST | Reset riwayat |

---

## Build dari source

### Daemon `agentd` (Go)

```bash
cd ai-agent-module/src
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w" -o ../module/bin/agentd main.go
```

### APK pendamping (Windows)

Jalankan `ai-agent-app\build.ps1`. Membutuhkan JDK 17, serta `aapt2`, `d8`, `zipalign`, dan `apksigner` dari Android SDK Build-Tools.

### Paket module

Bungkus isi folder `ai-agent-module/module` menjadi sebuah `.zip` (pastikan path di dalam zip memakai separator `/`).

---

## Struktur repo

```
AI-Agent/
├── ai-agent-module/       # module KernelSU/Magisk + source daemon
│   ├── module/            # isi zip module (config, scripts, apk, binary)
│   └── src/               # source Go (main.go + web UI)
└── ai-agent-app/          # source APK pendamping (Java)
```

---

## Keamanan

> **Penting.** Daemon ini berjalan sebagai **root** dan dapat menjalankan perintah shell serta mengubah file di HP kamu. Pasang hanya di perangkat yang kamu pahami risikonya. Set `allow_shell` ke `false` jika tidak memerlukan akses shell.

---

## Disclaimer

Proyek ini dibuat untuk keperluan belajar dan eksperimen. Gunakan dengan tanggung jawab sendiri. Penulis tidak bertanggung jawab atas kerusakan atau penyalahgunaan.

---

## Lisensi

MIT — lihat file `LICENSE`.

---

## Kredit

- Author: **ig:@zafxyra** + opencode

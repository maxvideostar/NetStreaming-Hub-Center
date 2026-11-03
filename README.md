# NetStreaming Hub Center (nTs-Suite)

Un singur program, un singur panou pe **portul 8090**, pentru toate cele 5 aplicații:

| Modul | Ce face | Porturi spre clienți |
|---|---|---|
| **nTs-relay** (StreamHub) | relay MPEG-TS tip Astra, PID-uri originale | 4000 (+ `listen_extra`) |
| **ProxyHub** | udpxy + relay canale HTTP-TS/HLS | 4001 implicit*, 7000 |
| **RTMP Media Server** | RTMP ingest, HLS/FLV/TS, push, pull, DVR | 1935, 8080 |
| **VideoEncoder** | encoder FFmpeg (QSV/AMF/x264) | – (panou doar prin suită) |
| **Rec-Martori** | înregistrări martor pe segmente | – (panou doar prin suită) |

\* La o instalare nouă ProxyHub pornește pe 4001 ca să nu se bată cu nTs-relay (4000). Dacă imporți configurația ta veche, se păstrează portul tău.

## Cum funcționează

- `nts-suite` conține în el executabilele celor 5 module (comprimate). La pornire le extrage în `data/bin` și le rulează ca procese supravegheate: repornire automată la cădere, oprire curată (inclusiv ffmpeg-urile pornite de ele), CPU/RAM per modul, jurnal.
- Un singur login (`admin/admin` la prima pornire – schimbă-l din **Setări**). Suita face singură login în fiecare modul cu o parolă internă generată aleator, deci panourile modulelor se deschid direct, fără a doua autentificare.
- Panourile modulelor sunt servite prin suită la `http://server:8090/m/<modul>/`. Streamurile rămân pe porturile lor (4000, 1935, 8080, 7000 …) – linkurile afișate în panouri arată portul corect.
- VideoEncoder și Rec-Martori ascultă doar pe `127.0.0.1` (18093/18094); sunt accesibile din afară numai prin panoul suitei.
- Fiecare modul poate fi pornit/oprit separat. Un modul pornit rămâne activat și pornește automat cu suita.

## Instalare

**Windows** – dezarhivează `nts-suite-windows-x64.zip` și rulează `Install-nTs-Suite.bat`. Instalează în `C:\nTs-Suite`, ca serviciu, dezactivează vechile servicii separate (ProxyHub, VideoEncoder, RecMartori, rtmp-media-server, StreamHub) ca să elibereze porturile și deschide firewall-ul. Pune `ffmpeg.exe` + `ffprobe.exe` în `C:\nTs-Suite\data\bin` (sau în PATH).

**Ubuntu** –
```bash
curl -fsSL https://raw.githubusercontent.com/maxvideostar/NetStreaming-Hub-Center/main/install.sh | sudo bash
```
Instalează în `/opt/nts-suite`, serviciu systemd `nTs-Suite`. Rulat din nou = actualizare.

## Mutarea configurațiilor existente

Setări → **Configurații module** → **Import** pentru fiecare modul, cu fișierul vechi:

| Modul | Fișier vechi |
|---|---|
| nTs-relay | `config.json` (lângă streamhub / `/etc/streamhub/`) |
| ProxyHub | `proxyhub.json` |
| RTMP Media Server | `config.json` |
| VideoEncoder | `config.json` (lângă VideoEncoder.exe) |
| Rec-Martori | `config.json` |

Canalele, streamurile, tokenurile și înregistrările se păstrează; suita ajustează doar parola internă și portul panoului. Configurația înlocuită rămâne ca `.bak-…`.

## Comenzi

```
nts-suite                       pornește în consolă
nts-suite install|uninstall     serviciu Windows / systemd
nts-suite start|stop|restart|status
nts-suite passwd <user> <parola>  resetează contul panoului
nts-suite firewall              (Windows) refă regulile de firewall
nts-suite -data DIR ...         alt director de date (implicit ./data)
```

## Structura `data/`

```
data/suite.json        cont, port, module activate
data/bin/              executabilele modulelor (+ ffmpeg dacă îl pui aici)
data/<modul>/          configurația și fișierele fiecărui modul
data/logs/             jurnalul suitei + jurnalul fiecărui modul
```

## Build

`./build.sh` clonează cele 5 repo-uri, le compilează și le include în `dist/nts-suite-<os>-<arch>`. Un tag `v*` pe GitHub face release automat (Windows x64, Linux amd64/arm64).

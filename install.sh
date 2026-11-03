#!/usr/bin/env bash
# nTs-Suite – instalare/actualizare pe Ubuntu/Debian (serviciu systemd, panou pe :8090)
#   curl -fsSL https://raw.githubusercontent.com/maxvideostar/NetStreaming-Hub-Center/main/install.sh | sudo bash
set -euo pipefail
DIR=/opt/nts-suite
REPO=maxvideostar/NetStreaming-Hub-Center
[ "$(id -u)" -eq 0 ] || { echo "Rulează cu sudo"; exit 1; }
case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "Arhitectură nesuportată: $(uname -m)"; exit 1 ;;
esac
apt-get install -y -qq curl ffmpeg >/dev/null 2>&1 || true
mkdir -p "$DIR"
URL="https://github.com/$REPO/releases/latest/download/nts-suite-linux-$ARCH"
if [ -f "./dist/nts-suite-linux-$ARCH" ]; then
  cp "./dist/nts-suite-linux-$ARCH" "$DIR/nts-suite.new"
else
  echo "Descarc $URL"
  curl -fL --retry 3 -o "$DIR/nts-suite.new" "$URL"
fi
chmod +x "$DIR/nts-suite.new"
if systemctl is-active --quiet nTs-Suite 2>/dev/null; then
  systemctl stop nTs-Suite
  mv -f "$DIR/nts-suite.new" "$DIR/nts-suite"
  systemctl start nTs-Suite
  echo "Actualizat și repornit."
else
  mv -f "$DIR/nts-suite.new" "$DIR/nts-suite"
  "$DIR/nts-suite" -data "$DIR/data" install
fi
if command -v ufw >/dev/null && ufw status | grep -q active; then
  ufw allow 8090/tcp >/dev/null
  echo "ufw: am deschis 8090 (deschide manual porturile modulelor: 4000, 1935, 8080, 7000 ...)"
fi
IP=$(hostname -I 2>/dev/null | awk '{print $1}')
echo
echo "nTs-Suite rulează:  http://${IP:-localhost}:8090   (prima autentificare: admin / admin)"
echo "Date:               $DIR/data"
echo "Jurnal:             journalctl -u nTs-Suite -f"

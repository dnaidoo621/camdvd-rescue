#!/usr/bin/env bash
# CamDVD Rescue installer.
#
#   curl -fsSL https://github.com/dnaidoo621/camdvd-rescue/releases/latest/download/install.sh | sudo bash
#
# Options (all optional; without them it asks):
#   --library DIR      where MP4s go (default /srv/camdvd/library)
#   --port N           web port (default 8780)
#   --network | --local  reachable from other machines, or this PC only
#   --group NAME       shared group for the library, e.g. for SMB
#   --hwaccel          add the service user to the render group (VAAPI)
#   --version vX.Y.Z   release to install (default: latest)
#   --bundle FILE      install a local camdvd-*.tar.gz instead of downloading
#   --reconfigure      ask the questions again on an existing install
#   --yes              accept defaults, ask nothing
#   --uninstall [--purge]  remove the app (--purge also config and database);
#                      the library is never touched
set -euo pipefail

REPO=${CAMDVD_REPO:-dnaidoo621/camdvd-rescue}
OPT=/opt/camdvd
ETC=/etc/camdvd
STATE=/var/lib/camdvd
UNIT=/etc/systemd/system/camdvd.service
UDEV=/etc/udev/rules.d/70-camdvd.rules

LIBRARY="" PORT="" NETWORK="" GROUP="" HWACCEL=0 VERSION="" BUNDLE="" YES=0 RECONF=0 UNINSTALL=0 PURGE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --library) LIBRARY=$2; shift ;;
    --port) PORT=$2; shift ;;
    --network) NETWORK=yes ;;
    --local) NETWORK=no ;;
    --group) GROUP=$2; shift ;;
    --hwaccel) HWACCEL=1 ;;
    --version) VERSION=$2; shift ;;
    --bundle) BUNDLE=$2; shift ;;
    --yes|-y) YES=1 ;;
    --reconfigure) RECONF=1 ;;
    --uninstall) UNINSTALL=1 ;;
    --purge) PURGE=1 ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    *) echo "Unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

say()  { printf '\033[1m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[33m!   %s\033[0m\n' "$*" >&2; }
die()  { printf '\033[31mxx  %s\033[0m\n' "$*" >&2; exit 1; }

# ask VAR "Question" default — reads from the terminal even when piped.
ask() {
  local var=$1 q=$2 def=$3 ans=""
  if [ "$YES" = 1 ] || [ ! -r /dev/tty ]; then
    printf -v "$var" '%s' "$def"; return
  fi
  read -r -p "$q [$def]: " ans </dev/tty || true
  printf -v "$var" '%s' "${ans:-$def}"
}

[ "$(id -u)" = 0 ] || die "Run as root: curl … | sudo bash"

# ---------------------------------------------------------------- uninstall
if [ "$UNINSTALL" = 1 ]; then
  say "Removing CamDVD Rescue"
  systemctl disable --now camdvd.service 2>/dev/null || true
  rm -f "$UNIT" "$UDEV" /usr/local/bin/camdvd
  systemctl daemon-reload
  udevadm control --reload 2>/dev/null || true
  rm -rf "$OPT"
  if [ "$PURGE" = 1 ]; then
    rm -rf "$ETC" "$STATE"
    say "Removed configuration and database"
  else
    say "Kept $ETC and $STATE (use --purge to remove them)"
  fi
  id camdvd >/dev/null 2>&1 && userdel camdvd 2>/dev/null || true
  say "Done. The library was not touched."
  exit 0
fi

# ---------------------------------------------------------------- 1. checks
say "Checking the system"
[ "$(uname -s)" = Linux ] || die "Linux only"
[ "$(uname -m)" = x86_64 ] || die "x86_64 (x64) only; this is $(uname -m)"
[ -d /run/systemd/system ] || die "systemd is required"
command -v apt-get >/dev/null || die "Only apt-based distros (Debian 12+, Ubuntu 22.04+) are supported in v1"
. /etc/os-release
supported=0
case "${ID:-}" in
  debian) [ "${VERSION_ID%%.*}" -ge 12 ] 2>/dev/null && supported=1 ;;
  *)
    if [ "${ID:-}" = ubuntu ] || [[ " ${ID_LIKE:-} " == *" ubuntu "* ]]; then
      case "${UBUNTU_CODENAME:-${VERSION_CODENAME:-}}" in
        jammy|noble|oracular|plucky|questing|resolute) supported=1 ;;
      esac
    elif [[ " ${ID_LIKE:-} " == *" debian "* ]]; then
      case "${DEBIAN_CODENAME:-${VERSION_CODENAME:-}}" in bookworm|trixie|forky) supported=1 ;; esac
    fi ;;
esac
[ "$supported" = 1 ] || die "${PRETTY_NAME:-This distro} isn't supported; need Debian 12+ or Ubuntu 22.04+"
echo "    ${PRETTY_NAME}"

# ---------------------------------------------------------------- 2. packages
say "Installing tools from the distro"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
pkgs=(gddrescue sg3-utils dvd+rw-tools libimage-exiftool-perl util-linux eject lsscsi curl ca-certificates)
if apt-cache show 7zip >/dev/null 2>&1; then pkgs+=(7zip); else pkgs+=(p7zip-full); fi
apt-get install -y -qq --no-install-recommends "${pkgs[@]}" >/dev/null

# ---------------------------------------------------------------- 3. bundle
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
if [ -n "$BUNDLE" ]; then
  [ -f "$BUNDLE" ] || die "$BUNDLE not found"
  if [ -f "$BUNDLE.sha256" ]; then
    (cd "$(dirname "$BUNDLE")" && sha256sum -c "$(basename "$BUNDLE").sha256" >/dev/null) || die "checksum mismatch for $BUNDLE"
  else
    warn "No $BUNDLE.sha256 next to the bundle; not verified"
  fi
  cp "$BUNDLE" "$TMP/bundle.tar.gz"
else
  if [ -z "$VERSION" ]; then
    VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
    [ -n "$VERSION" ] || die "Couldn't find the latest release of $REPO"
  fi
  name="camdvd-${VERSION#v}-linux-x64.tar.gz"
  base="https://github.com/$REPO/releases/download/$VERSION"
  say "Downloading $name"
  curl -fsSL -o "$TMP/bundle.tar.gz" "$base/$name"
  curl -fsSL -o "$TMP/bundle.sha256" "$base/$name.sha256"
  want=$(cut -d' ' -f1 "$TMP/bundle.sha256")
  have=$(sha256sum "$TMP/bundle.tar.gz" | cut -d' ' -f1)
  [ "$want" = "$have" ] || die "Checksum mismatch; download refused"
fi
tar -xzf "$TMP/bundle.tar.gz" -C "$TMP"
SRC=$(find "$TMP" -mindepth 1 -maxdepth 1 -type d -name 'camdvd-*' | head -1)
[ -n "$SRC" ] && [ -x "$SRC/camdvd" ] || die "Bundle is missing the camdvd binary"
VER=$(cat "$SRC/VERSION")
mkdir -p "$OPT"
rm -rf "${OPT:?}/$VER"
cp -a "$SRC" "$OPT/$VER"
chown -R root:root "$OPT/$VER" && chmod -R go-w "$OPT/$VER"
ln -sfn "$VER" "$OPT/.current.tmp" && mv -T "$OPT/.current.tmp" "$OPT/current"
ln -sfn "$OPT/current/camdvd" /usr/local/bin/camdvd
echo "    $OPT/$VER"

# ---------------------------------------------------------------- 4. user
say "Creating the camdvd service user"
if ! id camdvd >/dev/null 2>&1; then
  useradd --system --home-dir "$STATE" --shell /usr/sbin/nologin --user-group camdvd
fi
usermod -aG cdrom camdvd
extra_groups=""
if [ "$HWACCEL" = 1 ]; then
  for g in render video; do getent group "$g" >/dev/null && usermod -aG "$g" camdvd && extra_groups="$extra_groups $g"; done
fi
# -R: a reinstall may give the user a new UID; the old database must follow.
mkdir -p "$STATE" && chown -R camdvd:camdvd "$STATE" && chmod 750 "$STATE"

# ---------------------------------------------------------------- 5. drives
say "Looking for optical drives"
drives=$(lsscsi -g | awk '$2=="cd/dvd" && $(NF-1) ~ /^\/dev\/sr/ {print $(NF-1), $NF}')
if [ -z "$drives" ]; then
  warn "No optical drive found. Plug one in and re-run with --reconfigure."
else
  while read -r sr sg; do echo "    $sr ↔ $sg  ($(cat /sys/class/block/"${sr#/dev/}"/device/model 2>/dev/null | xargs))"; done <<<"$drives"
fi

# ---------------------------------------------------------------- 6. config
mkdir -p "$ETC"
if [ ! -f "$ETC/config.toml" ] || [ "$RECONF" = 1 ]; then
  say "A few questions"
  [ -n "$LIBRARY" ] || ask LIBRARY "Library folder for the MP4s" /srv/camdvd/library
  [ -n "$PORT" ] || ask PORT "Web port" 8780
  if [ -z "$NETWORK" ]; then
    ask net "Will other machines (e.g. a Mac) connect? yes/no" yes
    case "$net" in [Nn]*) NETWORK=no ;; *) NETWORK=yes ;; esac
  fi
  [ -n "$GROUP" ] || ask GROUP "Shared group for the library (blank for none)" ""
  case "$LIBRARY" in /*) ;; *) die "Library must be an absolute path" ;; esac
  [[ "$PORT" =~ ^[0-9]+$ ]] || die "Port must be a number"
  bind=127.0.0.1; [ "$NETWORK" = yes ] && bind=0.0.0.0
  {
    echo "# CamDVD Rescue configuration. Restart after editing: sudo systemctl restart camdvd"
    echo "# The login password lives in $ETC/env as CAMDVD_PASSWORD."
    echo "listen = \"$bind:$PORT\""
    echo "library = \"$LIBRARY\""
    echo "state_dir = \"$STATE\""
    echo "tools_dir = \"$OPT/current/bin\""
    echo "shared_group = \"$GROUP\""
    while read -r sr sg; do
      [ -n "$sr" ] || continue
      printf '\n[[drives]]\nid = "%s"\nblock = "%s"\nsg = "%s"\n' "${sr#/dev/}" "$sr" "$sg"
    done <<<"$drives"
  } >"$ETC/config.toml"
else
  say "Keeping the existing $ETC/config.toml (use --reconfigure to change it)"
  LIBRARY=$(sed -n 's/^library *= *"\(.*\)"/\1/p' "$ETC/config.toml")
  GROUP=$(sed -n 's/^shared_group *= *"\(.*\)"/\1/p' "$ETC/config.toml")
  bind=$(sed -n 's/^listen *= *"\(.*\):.*"/\1/p' "$ETC/config.toml")
  PORT=$(sed -n 's/^listen *= *".*:\([0-9]*\)"/\1/p' "$ETC/config.toml")
fi
if [ ! -f "$ETC/env" ]; then
  printf '# Set a password to require a login, then: sudo systemctl restart camdvd\n#CAMDVD_PASSWORD=\n' >"$ETC/env"
fi
chown root:camdvd "$ETC/env" && chmod 640 "$ETC/env"
chmod 644 "$ETC/config.toml"

mkdir -p "$LIBRARY"
if [ "$(stat -c %U "$LIBRARY")" != camdvd ] && [ -d "$LIBRARY/.camdvd" ]; then
  # An earlier install's files, from before the user was recreated.
  say "Re-owning the existing library for the new camdvd user"
  chown -R camdvd "$LIBRARY"
fi
if [ -n "$GROUP" ]; then
  getent group "$GROUP" >/dev/null || groupadd "$GROUP"
  usermod -aG "$GROUP" camdvd
  chown camdvd:"$GROUP" "$LIBRARY"
  chmod 2775 "$LIBRARY"
else
  chown camdvd:camdvd "$LIBRARY"
  chmod 755 "$LIBRARY"
fi
if [ "${bind:-}" = 0.0.0.0 ] && command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  ufw allow "$PORT/tcp" comment "CamDVD Rescue" >/dev/null && echo "    Opened port $PORT in ufw"
fi

# ---------------------------------------------------------------- 7. udev
say "Stopping desktop automount on the configured drives"
cp "$OPT/current/packaging/udev/70-camdvd.rules" "$UDEV"
grep -o 'block = "/dev/sr[0-9]*"' "$ETC/config.toml" | sed 's/.*\/dev\/\(sr[0-9]*\)"/\1/' | while read -r k; do
  echo "SUBSYSTEM==\"block\", KERNEL==\"$k\", ENV{UDISKS_IGNORE}=\"1\", ENV{UDISKS_AUTO}=\"0\"" >>"$UDEV"
done
udevadm control --reload && udevadm trigger --subsystem-match=block --sysname-match='sr*' || true

# ---------------------------------------------------------------- 8. service
say "Installing and starting camdvd.service"
protect=yes
case "$LIBRARY" in /home/*|/root/*) protect=no ;; esac
sed -e "s#@LIBRARY@#$LIBRARY#" -e "s#@PROTECT_HOME@#$protect#" -e "s#@EXTRA_GROUPS@#${extra_groups# }#" \
  "$OPT/current/packaging/systemd/camdvd.service" >"$UNIT"
systemctl daemon-reload
systemctl enable camdvd.service >/dev/null 2>&1
systemctl restart camdvd.service

say "Self-check"
if ! runuser -u camdvd -- "$OPT/current/camdvd" doctor -config "$ETC/config.toml"; then
  warn "Fix the lines above, then: sudo systemctl restart camdvd"
fi

host=localhost
if [ "${bind:-}" = 0.0.0.0 ]; then host=$(hostname -I 2>/dev/null | awk '{print $1}'); fi
echo
say "CamDVD Rescue $VER is running: http://$host:$PORT"
echo "    Logs: journalctl -u camdvd -f"

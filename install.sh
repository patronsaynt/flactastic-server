#!/usr/bin/env bash
# FLACtastic Server installer for Debian 13 (amd64, arm64).
#
# Read it before you run it:
#   curl -fsSLO https://raw.githubusercontent.com/patronsaynt/flactastic-server/main/install.sh
#   less install.sh
#   sudo bash install.sh
#
# Piping it into bash works too, but then you run whatever arrives. Either
# way, every file it downloads is checked against the release's SHA256SUMS.
set -Eeuo pipefail

DATA=/var/lib/flactastic-server
BIN=/usr/local/bin/flactastic-server
UNIT=/etc/systemd/system/flactastic-server.service
AVAHI=/etc/avahi/services/flactastic-server.service
PORT=4410
STEPS=8

usage() {
    cat <<EOF
Install or upgrade FLACtastic Server.

Usage: sudo bash install.sh [options]

  --yes               Run unattended and accept defaults
  --version TAG       Install this release (default: latest)
  --repo OWNER/NAME   Download from this repo (default: patronsaynt/flactastic-server)
  --binary PATH       Install a local binary instead of downloading one
  --sha256 HASH       Expected hash of --binary
  --admin NAME        First admin's username
  --library PATH      First admin's music folder
  --password-stdin    Read the first admin's password from stdin
  --no-ffmpeg         Skip ffmpeg (no transcoding)
  --no-avahi          Skip LAN discovery
  --no-firewall       Leave ufw alone
  --force             Install on something other than Debian 13
  --uninstall         Remove the server, keeping its data and your music
  --purge             Uninstall and delete the server's data and user
  -h, --help          Show this help

Install options can also be set as FLACTASTIC_<OPTION>, e.g. FLACTASTIC_YES=1,
FLACTASTIC_ADMIN=alice, FLACTASTIC_LIBRARY=/srv/music.

Safer than curl | bash: download install.sh, read it, then run it with sudo.
EOF
}

bool() { [[ ${1:-} =~ ^(1|yes|true)$ ]] && echo 1 || echo 0; }

YES=$(bool "${FLACTASTIC_YES:-}")
VERSION=${FLACTASTIC_VERSION:-latest}
REPO=${FLACTASTIC_REPO:-patronsaynt/flactastic-server}
BINARY=${FLACTASTIC_BINARY:-}
SHA256=${FLACTASTIC_SHA256:-}
ADMIN=${FLACTASTIC_ADMIN:-}
LIBRARY=${FLACTASTIC_LIBRARY:-}
PASSWORD_STDIN=$(bool "${FLACTASTIC_PASSWORD_STDIN:-}")
NO_FFMPEG=$(bool "${FLACTASTIC_NO_FFMPEG:-}")
NO_AVAHI=$(bool "${FLACTASTIC_NO_AVAHI:-}")
NO_FIREWALL=$(bool "${FLACTASTIC_NO_FIREWALL:-}")
FORCE=$(bool "${FLACTASTIC_FORCE:-}")
MODE=install

die() { printf '%serror:%s %s\n' "$RED" "$RESET" "$*" >&2; exit 1; }

if [[ -t 1 && -z ${NO_COLOR:-} && ${TERM:-dumb} != dumb ]]; then
    FANCY=1 BOLD=$'\e[1m' DIM=$'\e[2m' RED=$'\e[31m' GREEN=$'\e[32m' YELLOW=$'\e[33m' RESET=$'\e[0m'
else
    FANCY=0 BOLD='' DIM='' RED='' GREEN='' YELLOW='' RESET=''
fi

while (($#)); do
    case $1 in
        --yes) YES=1 ;;
        --version | --repo | --binary | --sha256 | --admin | --library)
            (($# > 1)) || die "$1 needs a value"
            declare "$(tr a-z- A-Z_ <<<"${1#--}")=$2"
            shift ;;
        --password-stdin) PASSWORD_STDIN=1 ;;
        --no-ffmpeg) NO_FFMPEG=1 ;;
        --no-avahi) NO_AVAHI=1 ;;
        --no-firewall) NO_FIREWALL=1 ;;
        --force) FORCE=1 ;;
        --uninstall) MODE=uninstall ;;
        --purge) MODE=purge ;;
        -h | --help) usage; exit 0 ;;
        *) die "unknown option $1 (see --help)" ;;
    esac
    shift
done

TMP=''
SPIN_PID=''
cleanup() {
    [[ -n $SPIN_PID ]] && kill "$SPIN_PID" 2>/dev/null
    [[ -n $TMP ]] && rm -rf "$TMP"
    return 0
}
trap cleanup EXIT
trap 'exit 130' INT TERM
trap 'printf "%serror:%s failed at line %s\n" "$RED" "$RESET" "$LINENO" >&2' ERR

STEP=0
step() { STEP=$((STEP + 1)); printf '\n%s[%d/%d]%s %s%s%s\n' "$DIM" "$STEP" "$STEPS" "$RESET" "$BOLD" "$1" "$RESET"; }
ok() { if ((FANCY)); then printf '  %s✓%s %s\n' "$GREEN" "$RESET" "$1"; else printf '  %s\n' "$1"; fi; }
warn() { printf '  %s!%s %s\n' "$YELLOW" "$RESET" "$1"; }
hint() { printf '    %s%s%s\n' "$DIM" "$1" "$RESET"; }

# run LABEL CMD... runs a long command quietly, showing its output only on failure.
run() {
    local label=$1 frames=(⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏) i=0 status=0
    shift
    if ((FANCY)); then
        "$@" >"$TMP/log" 2>&1 &
        SPIN_PID=$!
        while kill -0 "$SPIN_PID" 2>/dev/null; do
            printf '\r  %s%s%s %s' "$YELLOW" "${frames[i++ % 10]}" "$RESET" "$label"
            sleep 0.1
        done
        wait "$SPIN_PID" || status=$?
        SPIN_PID=''
        printf '\r\e[K'
    else
        "$@" >"$TMP/log" 2>&1 || status=$?
    fi
    if ((status)); then
        printf '  %sfailed:%s %s\n' "$RED" "$RESET" "$label"
        tail -n 20 "$TMP/log" >&2
        exit "$status"
    fi
    ok "$label"
}

can_prompt() { (: </dev/tty) 2>/dev/null; }

# ask QUESTION DEFAULT(y|n): under --yes, takes the default.
ask() {
    local reply hint='[y/N]'
    [[ $2 == y ]] && hint='[Y/n]'
    if ((YES)); then [[ $2 == y ]]; return; fi
    read -r -p "  $1 $hint " reply </dev/tty
    [[ ${reply:-$2} =~ ^[Yy] ]]
}

prompt() { local reply; read -r -p "  $1 " reply </dev/tty; printf '%s' "$reply"; }

valid_name() { [[ $1 =~ ^[a-z][a-z0-9_-]{0,31}$ ]]; }

valid_library() { [[ $1 == /* && $1 != *$'\n'* && -d $1 ]]; }

need_root() { ((EUID == 0)) || die "run this as root (sudo bash install.sh)"; }

# --- uninstall / purge -------------------------------------------------------

library_roots() {
    local conf=$UNIT.d/libraries.conf
    [[ -f $conf ]] || return 0
    sed -n 's/^ReadWritePaths=//p' "$conf" | xargs -r -n1 printf '%s\n' | sed 's/^-//'
}

uninstall() {
    need_root
    local lib libs=()
    if [[ $MODE == purge ]]; then
        ((YES)) || can_prompt || die "no terminal to ask on; use --yes to purge without touching music"
        mapfile -t libs < <(library_roots)
    fi
    systemctl disable --now flactastic-server.service >/dev/null 2>&1 || true
    rm -rf "$BIN" "$UNIT" "$UNIT.d" "$AVAHI"
    systemctl daemon-reload
    if command -v ufw >/dev/null && ufw show added | grep -q "allow $PORT/tcp"; then
        ufw delete allow "$PORT/tcp" >/dev/null
    fi
    ok "Removed the server, its service and discovery file"
    if [[ $MODE == uninstall ]]; then
        ok "Kept $DATA and your music"
        return
    fi
    if ((YES)) || ask "Delete $DATA (accounts, devices, indexes)? This can't be undone." n; then
        rm -rf "$DATA"
        getent passwd flactastic >/dev/null && userdel flactastic
        getent group flactastic >/dev/null && groupdel flactastic
        ok "Deleted $DATA and the flactastic user"
    else
        ok "Kept $DATA"
    fi
    for lib in "${libs[@]}"; do
        [[ -d $lib/.flactastic && ! -L $lib/.flactastic ]] || continue
        if ask "Remove $lib/.flactastic? Your music stays." n; then
            rm -rf "${lib:?}/.flactastic"
            ok "Removed $lib/.flactastic"
        fi
    done
    ok "Your music files are untouched"
}

if [[ $MODE != install ]]; then
    uninstall
    exit 0
fi

# --- install -----------------------------------------------------------------

need_root
((YES)) || can_prompt || die "no terminal to ask on; re-run with --yes (plus --admin, --library and --password-stdin for the first account)"
[[ -z $ADMIN ]] || valid_name "$ADMIN" || die "--admin must match ^[a-z][a-z0-9_-]{0,31}\$"
[[ -z $LIBRARY ]] || valid_library "$LIBRARY" || die "--library must be an existing folder, as an absolute path"
[[ -z $BINARY || -f $BINARY ]] || die "--binary $BINARY doesn't exist"
[[ $REPO =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die "--repo must look like OWNER/NAME"
TMP=$(mktemp -d)

logo() {
    local cols
    ((FANCY)) || return 0
    cols=$({ stty size </dev/tty; } 2>/dev/null | cut -d' ' -f2) || cols=0
    ((${cols:-0} >= 66)) || return 0
    cat <<'ART'
                                 ░░▒
                          ░▒▒▓▓▓████░
                    ░▒▓▓█████▓▓▓▓▓▓█▓
            ░▓▓▓▓▓▓▓▓▓▓▓████▓▓▓▓▓▓▓▓▓▓▓▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒░
                       ░▓██▓▓▓▓▓▓▓▓▓▓░
                        ▓█▓▓▓▓▓▓▓▓▓▓█▒
                        ▓▓▓▓▓▓▓▓▓▓▓▓▓▓▒▒▒▒▒▒▒▒▒▒▒░░░
                       ░▓▓▓▓▓▓▓▓▓▓▓▓▓░
                       ░▓▓▓▓▓▓▓▓▓▓▓▓▓░
                       ░▓▓▓▓▓▓▓▓▓▓▓▓▓░
                       ▒▓▓▓▓▓▓▓▓▓▓▓▓▓░
                       ▒▓▓▓▓▓▓▓▓▓▓▓▓▓░
                       ▒▓▓▓▓▓▓▓▓▓▓▒▒▓▓░
                         ░▒▒▓▓▓▓▓▒▒▒▒▒▓▓▒░                     ░▒░
                                 ░░░░░░░░▒▒▒▒▒▒▒▒░░░░░░░░
ART
    echo
}

logo
printf '%sFLACtastic Server%s\n' "$BOLD" "$RESET"

step "Checking this system"
# shellcheck source=/dev/null # the host's own file, not part of this script
os=$(. /etc/os-release && echo "${ID:-}:${VERSION_ID:-}:${VERSION_CODENAME:-}")
if [[ $os == debian:13:* || $os == debian::trixie ]]; then
    ok "Debian 13"
elif ((FORCE)); then
    warn "Not Debian 13 ($os); continuing because of --force"
else
    die "this installer supports Debian 13 only (found $os); --force to try anyway"
fi
[[ -d /run/systemd/system ]] || die "systemd isn't running"
ARCH=$(dpkg --print-architecture)
[[ $ARCH == amd64 || $ARCH == arm64 ]] || die "unsupported architecture $ARCH"
ok "$ARCH, systemd"
for dir in /usr/local /var/lib; do
    free=$(df -Pk "$dir" | awk 'NR==2 {print $4}')
    ((free >= 204800)) || die "$dir has $((free / 1024)) MB free; 200 MB needed"
done
ok "Enough free space"

step "Installing packages"
pkgs=(ca-certificates curl)
((NO_FFMPEG)) || pkgs+=(ffmpeg)
((NO_AVAHI)) || pkgs+=(avahi-daemon)
missing=()
for p in "${pkgs[@]}"; do
    dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q 'ok installed' || missing+=("$p")
done
if ((${#missing[@]})); then
    export DEBIAN_FRONTEND=noninteractive
    run "Updating package lists" apt-get update
    run "Installing ${missing[*]}" apt-get install -y --no-install-recommends "${missing[@]}"
else
    ok "${pkgs[*]} already installed"
fi

step "Installing flactastic-server"
fetch() {
    curl -fsSL --proto '=https' --tlsv1.2 --retry 3 -o "$TMP/$1" \
        "https://github.com/$REPO/releases/download/$VERSION/$1"
}
verify() {
    local want
    want=$(awk -v f="$1" '$2 == f || $2 == "*" f {print $1; exit}' "$TMP/SHA256SUMS")
    [[ -n $want ]] || die "$1 isn't listed in SHA256SUMS"
    [[ $(sha256sum <"$TMP/$1") == "$want  -" ]] || die "$1 doesn't match SHA256SUMS"
}
# Fetched whole before searching: grep leaving early would fail curl, and with it the pipeline.
tag_of() {
    local json
    json=$(curl -fsSL --proto '=https' "$1" 2>/dev/null) || return
    grep -m1 -o '"tag_name": *"[^"]*"' <<<"$json" | cut -d'"' -f4
}
download() { for f in SHA256SUMS "${assets[@]}"; do fetch "$f" || return; done; }

here=''
[[ -f ${BASH_SOURCE[0]:-} ]] && here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
assets=()
if [[ -n $here && -f $here/dist/flactastic-server.service && -f $here/dist/flactastic-server.avahi.service ]]; then
    cp "$here/dist/flactastic-server.service" "$here/dist/flactastic-server.avahi.service" "$TMP/"
else
    assets+=(flactastic-server.service flactastic-server.avahi.service)
fi
[[ -n $BINARY ]] || assets+=("flactastic-server-linux-$ARCH")

if ((${#assets[@]})); then
    if [[ $VERSION == latest ]]; then
        api=https://api.github.com/repos/$REPO/releases
        VERSION=$(tag_of "$api/latest" || tag_of "$api?per_page=1") || VERSION=''
    fi
    [[ $VERSION =~ ^[A-Za-z0-9._-]+$ ]] || die "couldn't find a release in $REPO"
    run "Downloading $VERSION from $REPO" download
    for f in "${assets[@]}"; do verify "$f"; done
    ok "Checked against SHA256SUMS"
fi

if [[ -n $BINARY ]]; then
    cp "$BINARY" "$TMP/flactastic-server-linux-$ARCH"
    hash=$(sha256sum <"$TMP/flactastic-server-linux-$ARCH" | cut -d' ' -f1)
    if [[ -n $SHA256 ]]; then
        [[ $hash == "${SHA256,,}" ]] || die "$BINARY doesn't match --sha256"
        ok "Local binary matches --sha256"
    else
        warn "Local binary, not verified. SHA-256: $hash"
    fi
fi
install -m 0755 -o root -g root "$TMP/flactastic-server-linux-$ARCH" "$BIN.new"
new_version=$("$BIN.new" version) || { rm -f "$BIN.new"; die "the new binary doesn't run on this system"; }
mv -f "$BIN.new" "$BIN"
ok "Installed $BIN ($new_version)"

step "Starting the service"
getent group flactastic >/dev/null || groupadd --system flactastic
getent passwd flactastic >/dev/null ||
    useradd --system --gid flactastic --home-dir "$DATA" --no-create-home --shell /usr/sbin/nologin flactastic
install -d -m 0750 -o flactastic -g flactastic "$DATA"
ok "User flactastic, data in $DATA"
install -m 0644 -o root -g root "$TMP/flactastic-server.service" "$UNIT"
systemctl daemon-reload
systemctl enable flactastic-server.service >/dev/null 2>&1
systemctl restart flactastic-server.service
systemctl is-active --quiet flactastic-server.service ||
    die "the service didn't start; see: journalctl -u flactastic-server"
ok "flactastic-server.service is running"

step "First account"
USERNAME=''
users=$("$BIN" user list)
if [[ -n $users ]]; then
    ok "Accounts already exist; skipping"
elif ((YES)) && [[ -z $ADMIN ]]; then
    warn "Skipped; add one later: sudo flactastic-server user add NAME --root PATH --admin"
else
    ((YES)) && { [[ -n $LIBRARY ]] || die "--admin needs --library"; ((PASSWORD_STDIN)) || die "--yes needs --password-stdin to set the password"; }
    while [[ -z $ADMIN ]]; do
        default=''
        valid_name "${SUDO_USER:-}" && default=$SUDO_USER
        ADMIN=$(prompt "Admin username${default:+ [$default]}:")
        ADMIN=${ADMIN:-$default}
        valid_name "$ADMIN" || { warn "Use lowercase letters, digits, - or _ (start with a letter)."; ADMIN=''; }
    done
    while :; do
        while [[ -z $LIBRARY ]]; do
            LIBRARY=$(prompt "Music folder for $ADMIN:")
            valid_library "$LIBRARY" || { warn "Enter an existing folder as an absolute path."; LIBRARY=''; }
        done
        LIBRARY=$(realpath -e "$LIBRARY")
        read -r fstype target < <(findmnt -n -o FSTYPE,TARGET --target "$LIBRARY")
        if mountpoint -q "$LIBRARY"; then ok "$LIBRARY is a $fstype mount"; else ok "$LIBRARY is on $target ($fstype)"; fi
        if [[ $fstype =~ ^(nfs|nfs4|cifs|smb3|fuse) ]]; then
            warn "Network share: make sure it mounts at boot (fstab with _netdev, or a systemd mount unit)."
        fi
        cannot=''
        runuser -u flactastic -- test -r "$LIBRARY" -a -x "$LIBRARY" || cannot='read'
        runuser -u flactastic -- test -w "$LIBRARY" || cannot+="${cannot:+ or }write"
        [[ -z $cannot ]] && break
        warn "The flactastic user can't $cannot $LIBRARY ($(stat -c '%U:%G %A' "$LIBRARY"))."
        hint "Grant access with an ACL: setfacl -R -m u:flactastic:rwX,d:u:flactastic:rwX '$LIBRARY'"
        group=$(stat -c %G "$LIBRARY")
        [[ $group == root ]] || hint "or add flactastic to its group: usermod -aG $group flactastic"
        ((YES)) && break
        ask "Use it anyway?" n && break
        LIBRARY=''
    done
    if ((PASSWORD_STDIN)); then
        "$BIN" user add "$ADMIN" --root "$LIBRARY" --admin --password-stdin >"$TMP/log" 2>&1 ||
            { cat "$TMP/log" >&2; die "couldn't create $ADMIN"; }
    else
        { set +x; } 2>/dev/null
        while :; do
            read -r -s -p "  Password for $ADMIN: " pw </dev/tty; echo
            read -r -s -p "  Again: " pw2 </dev/tty; echo
            [[ -n $pw && $pw == "$pw2" ]] && break
            warn "Passwords didn't match or were empty."
        done
        printf '%s\n' "$pw" | "$BIN" user add "$ADMIN" --root "$LIBRARY" --admin --password-stdin >"$TMP/log" 2>&1 ||
            { unset pw pw2; cat "$TMP/log" >&2; die "couldn't create $ADMIN"; }
        unset pw pw2
    fi
    USERNAME=$ADMIN
    ok "Created admin $ADMIN with $LIBRARY"
fi

"$BIN" info >"$TMP/info"
field() { sed -n "s/^$1: //p" "$TMP/info"; }
ID=$(field id)
[[ $ID =~ ^[0-9a-fA-F-]{36}$ ]] || die "flactastic-server info gave no valid id"

step "LAN discovery"
if ((NO_AVAHI)); then
    rm -f "$AVAHI"
    ok "Skipped (--no-avahi)"
else
    name=$(field name)
    name=${name//&/"&amp;"} name=${name//</"&lt;"} name=${name//>/"&gt;"}
    xml=$(<"$TMP/flactastic-server.avahi.service")
    xml=${xml//@ID@/"$ID"} xml=${xml//@NAME@/"$name"}
    printf '%s\n' "$xml" >"$TMP/avahi.xml"
    install -m 0644 -o root -g root "$TMP/avahi.xml" "$AVAHI"
    ok "Advertising _flactastic-srv._tcp on port $PORT"
fi

step "Firewall"
if ((NO_FIREWALL)); then
    ok "Skipped (--no-firewall)"
elif ! command -v ufw >/dev/null || ! ufw status | grep -q '^Status: active'; then
    ok "ufw isn't active; nothing to do"
elif ask "Allow $PORT/tcp through ufw?" y; then
    ufw allow "$PORT/tcp" comment 'FLACtastic Server' >/dev/null
    ok "ufw allows $PORT/tcp"
else
    warn "Not changed; clients need $PORT/tcp open"
fi

step "Self-test"
for _ in {1..30}; do
    curl -fsk --max-time 2 "https://127.0.0.1:$PORT/v2/server" >"$TMP/server.json" 2>/dev/null && break
    sleep 1
done
grep -q "\"id\": *\"$ID\"" "$TMP/server.json" ||
    die "https://127.0.0.1:$PORT/v2/server didn't answer as this server; see: journalctl -u flactastic-server"
ok "Answering on https://127.0.0.1:$PORT"
FINGERPRINT=$(field fingerprint)
if command -v openssl >/dev/null; then
    hex=$(timeout 10 openssl s_client -connect "127.0.0.1:$PORT" </dev/null 2>/dev/null |
        openssl x509 -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum) ||
        die "couldn't read the live certificate"
    bits=$((16#${hex:0:15})) b32=ABCDEFGHIJKLMNOPQRSTUVWXYZ234567 seen=''
    for i in {11..0}; do seen+=${b32:$(((bits >> (5 * i)) & 31)):1}; done
    seen=${seen:0:4}-${seen:4:4}-${seen:8:4}
    [[ $seen == "$FINGERPRINT" ]] || die "the live certificate ($seen) doesn't match info ($FINGERPRINT)"
    ok "Certificate matches fingerprint $FINGERPRINT"
else
    warn "openssl isn't installed; skipped the certificate check"
fi

lines=()
while IFS= read -r a; do lines+=("Address      $a"); done < <(field address)
lines+=("Fingerprint  $FINGERPRINT" "             Confirm it in the app the first time you connect.")
[[ -z $USERNAME ]] || lines+=("Username     $USERNAME")
lines+=("Version      $new_version" "Logs         journalctl -u flactastic-server")
echo
if ((FANCY)); then
    width=0
    for l in "${lines[@]}"; do ((${#l} > width)) && width=${#l}; done
    printf -v bar '%*s' "$((width + 2))" ''
    bar=${bar// /─}
    printf '%s╭%s╮%s\n' "$GREEN" "$bar" "$RESET"
    printf '%s│%s %s%-*s%s %s│%s\n' "$GREEN" "$RESET" "$BOLD" "$width" "FLACtastic Server is ready" "$RESET" "$GREEN" "$RESET"
    for l in "${lines[@]}"; do printf '%s│%s %-*s %s│%s\n' "$GREEN" "$RESET" "$width" "$l" "$GREEN" "$RESET"; done
    printf '%s╰%s╯%s\n' "$GREEN" "$bar" "$RESET"
else
    echo "FLACtastic Server is ready"
    printf '  %s\n' "${lines[@]}"
fi

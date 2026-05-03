#!/bin/sh
# pkg-postinstall.sh — post-install script for the clustr-serverd RPM package.
#
# Run by the package manager after binary and config files are placed.
# Must be idempotent: safe to run on both fresh install and upgrade.

set -e

# ---------------------------------------------------------------------------
# Create clustr system user and group
# ---------------------------------------------------------------------------
# The service runs as root (required by nspawn/loop/DHCP capabilities) but
# data directories are owned by the clustr user so that files written by
# non-root subprocesses land under a predictable identity. This also makes
# the ownership story clean for backups and audits.

if ! getent group clustr > /dev/null 2>&1; then
    groupadd --system clustr
fi

if ! getent passwd clustr > /dev/null 2>&1; then
    useradd \
        --system \
        --gid clustr \
        --no-create-home \
        --home-dir /var/lib/clustr \
        --shell /sbin/nologin \
        --comment "clustr server" \
        clustr
fi

# ---------------------------------------------------------------------------
# Add ldap user to the clustr group so slapd can traverse /var/lib/clustr/ldap/
# ---------------------------------------------------------------------------
# /var/lib/clustr/ldap/ is mode 750 root:clustr.  slapd runs as user `ldap`
# (provided by openldap-servers, which is installed when LDAP is enabled).
# Without group membership, slapd cannot traverse the directory to reach its
# data dir and crash-loops with EACCES.
#
# We add ldap to the clustr group here unconditionally.  If the `ldap` user
# does not exist yet (openldap-servers not installed), getent returns nothing
# and usermod is skipped — safe no-op.  Once openldap-servers is installed,
# re-running `dnf reinstall clustr-serverd` or the one-shot recovery command
# below will apply the membership.
#
# We chose group membership over chmod 755 on the parent directory because
# 750 root:clustr + ldap-in-clustr means only processes in the clustr group
# can traverse the dir.  chmod 755 would allow any user on the host to enter
# the parent — weaker than necessary given the ldap data sub-dir holds
# directory service credentials.
if getent passwd ldap > /dev/null 2>&1; then
    usermod -aG clustr ldap
fi

# ---------------------------------------------------------------------------
# Fix ownership on data and log directories
# ---------------------------------------------------------------------------
chown -R root:clustr /var/lib/clustr
chown -R root:clustr /var/log/clustr
chown -R root:clustr /etc/clustr

# ---------------------------------------------------------------------------
# Apply setuid bit to clustr-privhelper
# ---------------------------------------------------------------------------
# nfpm cannot write the setuid bit in RPM file metadata, so we apply it here.
# This block is idempotent: chmod 4755 on an already-4755 file is a no-op.
# The binary must exist at this point — it is laid down by the RPM payload
# before %post runs.  If it is somehow absent (e.g. partial install), the
# error from chmod will surface as a post-install failure, which is correct
# behaviour — do not silently skip a missing suid binary.
chmod 4755 /usr/sbin/clustr-privhelper

# ---------------------------------------------------------------------------
# SELF-MON (#243): Seed /etc/clustr/rules.d/ with the built-in rule files
# if the target files are absent.  We never overwrite operator edits.
# ---------------------------------------------------------------------------
RULES_SRC_DIR=/usr/share/clustr/rules
RULES_DST_DIR=/etc/clustr/rules.d

# Ensure the rules.d directory exists.
if [ ! -d "$RULES_DST_DIR" ]; then
    mkdir -p "$RULES_DST_DIR"
    chown root:clustr "$RULES_DST_DIR"
    chmod 750 "$RULES_DST_DIR"
fi

# Copy each shipped rule file only if the destination doesn't exist.
for src in "$RULES_SRC_DIR"/*.yaml "$RULES_SRC_DIR"/*.yml; do
    [ -f "$src" ] || continue
    dst="$RULES_DST_DIR/$(basename "$src")"
    if [ ! -f "$dst" ]; then
        cp "$src" "$dst"
        chmod 640 "$dst"
        chown root:clustr "$dst"
    fi
done

# Enable the selfmon watchdog timer on first install so operators don't have to
# remember to enable it manually.  We use --now to start it immediately, but
# suppress errors: if clustr-serverd isn't running yet, the timer will fire
# naturally once systemd processes the new units after daemon-reload.
# ---------------------------------------------------------------------------
# Reload systemd unit database
# ---------------------------------------------------------------------------
if command -v systemctl > /dev/null 2>&1; then
    systemctl daemon-reload || true
    # Enable selfmon watchdog timer (idempotent).
    systemctl enable clustr-selfmon-watchdog.timer 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# Post-install notice
# ---------------------------------------------------------------------------
echo ""
echo "clustr-serverd installed."
echo ""
echo "Before starting the service:"
echo "  1. Edit /etc/clustr/clustr-serverd.conf"
echo "     Set CLUSTR_PXE_INTERFACE and CLUSTR_PXE_SERVER_IP for your"
echo "     provisioning network, then set CLUSTR_PXE_ENABLED=true."
echo ""
echo "  2. Create /etc/clustr/secrets.env with a persistent session secret:"
echo "       openssl rand -hex 64 | sed 's/^/CLUSTR_SESSION_SECRET=/' \\"
echo "         > /etc/clustr/secrets.env"
echo "       chmod 0400 /etc/clustr/secrets.env"
echo ""
echo "  3. Enable and start the service:"
echo "       systemctl enable --now clustr-serverd"
echo ""
echo "  4. Create the admin account (run once, on this host):"
echo "       clustr-serverd bootstrap-admin"
echo "     Default credentials: clustr / clustr"
echo "     You will be prompted to change the password on first login."
echo ""

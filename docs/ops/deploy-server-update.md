# clonr-server Deploy Procedure

Covers: SSH hardening rollout, binary update, auth key bootstrap.

---

## SSH Hardening (99-clonr-hardening.conf)

The hardening drop-in lives at `deploy/ssh/99-clonr-hardening.conf`. It must be
applied manually to the host after confirming key-based SSH access works.

### Prerequisites

Before disabling password auth, verify your SSH key is in `~/.ssh/authorized_keys`
for the user you will use to reconnect:

```bash
# Verify key auth works WITHOUT disabling passwords first
ssh -o PasswordAuthentication=no -i ~/.ssh/your_key clonr@192.168.1.151 'echo KEY_OK'
```

Only proceed if that returns `KEY_OK`.

### Apply

```bash
# 1. Copy drop-in to host
scp deploy/ssh/99-clonr-hardening.conf root@192.168.1.151:/etc/ssh/sshd_config.d/

# 2. Validate config syntax
ssh root@192.168.1.151 'sshd -t && echo CONFIG_VALID'

# 3. Reload sshd (does NOT drop current sessions)
ssh root@192.168.1.151 'systemctl reload sshd'

# 4. Verify effective config
ssh root@192.168.1.151 'sshd -T | grep -E "permitroot|passwordauth|pubkeyauth|x11forward|maxauthtries|logingrace"'
```

Expected output after step 4:
```
permitrootlogin prohibit-password
passwordauthentication no
pubkeyauthentication yes
x11forwarding no
maxauthtries 3
logingracetime 20
```

### Lockout recovery

If locked out: access via Proxmox VNC console (Proxmox API termproxy or web UI).
VM is at VMID 200 on pve (192.168.1.223). Edit /etc/ssh/sshd_config.d/99-clonr-hardening.conf
and reload sshd from the console.

---

## Binary Update (auth-enabled clonr-serverd)

Dinesh's auth commit (a4802c1) is in the repo. The running binary predates it.

### Build and deploy

```bash
# On a build host with Go 1.24:
cd staging/clonr
make server                   # outputs bin/clonr-serverd

# Transfer to clonr-server host
scp bin/clonr-serverd root@192.168.1.151:/usr/local/bin/clonr-serverd.new
ssh root@192.168.1.151 'chmod +x /usr/local/bin/clonr-serverd.new && \
  systemctl stop clonr-serverd && \
  mv /usr/local/bin/clonr-serverd /usr/local/bin/clonr-serverd.prev && \
  mv /usr/local/bin/clonr-serverd.new /usr/local/bin/clonr-serverd && \
  systemctl start clonr-serverd'
```

### Generate admin key

After restarting with the new binary, the first start bootstraps the api_keys table.
Generate an admin key:

```bash
ssh root@192.168.1.151 '/usr/local/bin/clonr-serverd apikey create --scope=admin \
  --description="bootstrap admin key"'
```

Save the output key to `/home/ubuntu/sqoia-dev-secrets/clonr/admin-api-key`.
The key is shown exactly once.

### Verify auth enforcement

```bash
# Without key: should return 401
curl -sk http://192.168.1.151:8080/api/v1/nodes

# With key: should return 200
curl -sk -H "Authorization: Bearer <key>" http://192.168.1.151:8080/api/v1/nodes
```

### Update systemd unit (if CLONR_AUTH_DEV_MODE was set)

Verify the unit file has NO `CLONR_AUTH_DEV_MODE` line:

```bash
grep -i dev_mode /usr/local/lib/systemd/system/clonr-serverd.service || echo "CLEAN"
```

The unit in `deploy/systemd/clonr-serverd.service` has no dev-mode line — it is
auth-enforcing by default. If `CLONR_AUTH_DEV_MODE=1` was added ad-hoc on the host,
remove it and reload the unit.

---

## CLONR_AUTH_TOKEN (legacy)

The old single pre-shared token is fully replaced by the scoped api_keys table in
commit a4802c1. Any `CLONR_AUTH_TOKEN` env var in the unit file is dead config and
should be removed.

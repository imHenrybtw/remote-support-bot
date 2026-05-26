# 🤖 remote-support-bot

[![Build](https://github.com/imHenrybtw/remote-support-bot/actions/workflows/build.yml/badge.svg)](https://github.com/imHenrybtw/remote-support-bot/actions/workflows/build.yml)
[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat&logo=go&logoColor=white)](https://golang.org)
[![License](https://img.shields.io/badge/License-GPL--3.0-blue?style=flat)](LICENSE)
![Status](https://img.shields.io/badge/Status-In%20Development-orange?style=flat)

> IT support bot that assists **remote employees** directly via WhatsApp — no corporate network access required.

---

## 🎯 The problem this project solves

Remote employees working from home are outside the internal network. When they face access issues — AD account locked, firewall session stuck, VPN not connecting — they can't open a ticket through the internal portal or access the intranet. The only channel that **always works** is their phone.

Without this bot, handling these situations depends on:

- Calling an internal extension (unreachable from outside)
- Email, which requires the user to be logged in (impossible if the account is locked)
- Accessing GLPI/ServiceDesk via VPN (can't open VPN if the session is broken)

The result: the employee is stuck waiting for someone on the IT team to respond manually.

This bot eliminates that dependency — the user sends a WhatsApp message and resolves the issue **without human intervention** for the most common cases.

---

## ✅ Features

| Feature | Description |
|---|---|
| 🔓 **AD Account Unlock** | Locates the account in Active Directory, sends a confirmation code by email, and unlocks it automatically |
| 🔥 **Firewall Session Termination** | Queries all configured FortiGate firewalls, locates active sessions, and terminates them via API |
| 🌐 **Step-by-step VPN Guide** | Sends an illustrated tutorial with FortiClient screenshots |
| 🧑‍💼 **Human Escalation** | Opens a ticket in GLPI, notifies the team in Teams, and monitors SLA response time |
| ⏱️ **Automatic Timeouts** | Notifies the user if no one responded within SLA and closes inactive sessions |
| 🔐 **Access Whitelist** | Only registered remote employees can interact with the bot |

---

## 🔐 Security — Phone Whitelist

The bot maintains an `allowed_phones` table in PostgreSQL with authorized phone numbers. Messages from unregistered numbers are **silently discarded** — no response is sent, to avoid confirming the service exists.

### Why this matters

- The bot's WhatsApp number may be accidentally shared
- Without a whitelist, anyone could trigger the account unlock or firewall session termination flow
- The whitelist prevents even the start of any flow for unauthorized users

### Authorization flow

```
Incoming message
        │
        ▼
IsPhoneAllowed(phone)?  ──── NO ────▶  silently discarded (fail-closed)
        │
       YES
        │
        ▼
bot.Handle(...)  ──▶  normal response
```

The check uses a partial index `WHERE active = TRUE`, ensuring O(log n) lookup.

### Managing the whitelist

**Add manually via SQL:**
```sql
INSERT INTO allowed_phones (phone, matricula, name)
VALUES ('11991234567', 'JSMITH', 'John Smith')
ON CONFLICT (phone) DO UPDATE
    SET active = TRUE, name = EXCLUDED.name, updated_at = NOW();
```

**Deactivate (soft-delete):**
```sql
UPDATE allowed_phones SET active = FALSE, updated_at = NOW()
WHERE phone = '11991234567';
```

**Via Go code (HR system integration):**
```go
// Add individually
db.AddAllowedPhone("11991234567", "JSMITH", "John Smith")

// Bulk sync — atomically replaces the entire list
entries := []db.AllowedPhone{
    {Phone: "11991234567", Matricula: "JSMITH", Name: "John Smith"},
    {Phone: "47987654321", Matricula: "MMATOS", Name: "Maria Matos"},
}
db.BulkSyncAllowedPhones(entries)
```

> **Tip:** Schedule `BulkSyncAllowedPhones` to run daily, fed by remote employees from your HR system or an AD group (e.g., `GRP_HomeOffice`).

---

## 🏗️ Architecture

```
WhatsApp (whatsmeow)
        │
        ▼
  eventHandler
        │
        ├── IsPhoneAllowed()  ◀── PostgreSQL: allowed_phones (whitelist)
        │
        ├── bot.Handle()      ◀── Per-session state machine
        │       │
        │       ├── AD (LDAP/LDAPS)          → account unlock
        │       ├── FortiGate (REST API)     → firewall session termination
        │       ├── GLPI (OAuth2 REST API)   → ticket management
        │       ├── SMTP                     → confirmation code delivery
        │       └── Teams (Webhook)          → team notifications
        │
        └── startCheckers()   → timeout goroutine (1 min interval)
```

---

## ⚙️ Installation

### Prerequisites

- Go 1.22+
- PostgreSQL 14+
- Network access to AD (LDAP/LDAPS), FortiGate, and GLPI

### 1. Clone and build

```bash
git clone https://github.com/imHenrybtw/remote-support-bot
cd support-bot
go build -o support-bot ./...
```

### 2. Configure credentials

```bash
cp .env.example .env
# Edit .env and fill in all values
```

### 3. Configure `config.yaml`

Adjust the values to match your environment. Key fields:

```yaml
bot:
  company_name: "My Company"   # appears in emails and Teams cards

ad:
  host:    "ldaps://dc.company.local:636"
  bind_dn: "CN=ldap bot,OU=Services,DC=company,DC=local"
  base_dn: "DC=company,DC=local"

smtp:
  host: "192.168.1.10"
  from: "IT Support Bot <no-reply@company.com>"

firewalls:
  - name: "HQ"              # FW_TOKEN_HQ in .env
    host: "https://fortigate.company.com"

glpi:
  url:          "https://glpi.company.com/api.php/v2"
  ticket_title: "WhatsApp Support"

vpn:
  gateway:      "ssl-vpn.company.com"
  port:         8443
  profile_name: "Company-VPN"
  images_path:  "/opt/support-bot/vpn-images"
```

> **VPN:** The tutorial sent to users uses `gateway`, `port`, and `profile_name` directly from `config.yaml`. Just update these values to adapt to your environment — no recompilation needed.

### 4. Create the database

```bash
psql -U postgres -c "CREATE USER botsuporte WITH PASSWORD 'your_password';"
psql -U postgres -c "CREATE DATABASE support_bot OWNER botsuporte;"
```

Tables are created automatically on first run.

### 5. Populate the whitelist

```bash
psql -U botsuporte -d support_bot -c "
INSERT INTO allowed_phones (phone, matricula, name) VALUES
  ('11991234567', 'JSMITH', 'John Smith'),
  ('47987654321', 'MMATOS', 'Maria Matos');
"
```

### 6. Run

```bash
./support-bot config.yaml
```

On first run (no saved session), a QR Code appears in the terminal. Scan it with WhatsApp via **Linked Devices → Link a Device**.

### 7. Install as a systemd service

```bash
# Create a dedicated user (no shell, no home directory)
sudo useradd -r -s /sbin/nologin support-bot

# Copy files
sudo mkdir -p /opt/support-bot
sudo cp support-bot config.yaml .env /opt/support-bot/
sudo chown -R support-bot:support-bot /opt/support-bot
sudo chmod 600 /opt/support-bot/.env   # owner read-only for credentials

# Install and start the service
sudo cp support-bot.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now support-bot

# Follow logs in real time
sudo journalctl -u support-bot -f
```

---

## 📁 Project structure

```
.
├── main.go               # Entry point, WhatsApp handler, whitelist guard
├── config.go             # config.yaml + .env reader
├── config.yaml           # Configuration (safe to commit)
├── .env.example          # Credentials template (never commit .env)
├── .gitignore
├── db.go                 # PostgreSQL: migrations, CRUD, whitelist
├── ad.go                 # LDAP/LDAPS client for Active Directory
├── fortigate.go          # FortiGate REST API (sessions and deauth)
├── glpi.go               # GLPI REST API with OAuth2
├── mail.go               # Transactional email delivery (SMTP)
├── teams.go              # Microsoft Teams notifications (Adaptive Cards)
├── vpn.go                # VPN guide with images (configurable via YAML)
└── support-bot.service   # systemd unit file
```

---

## 🗄️ Database schema

| Table | Purpose |
|---|---|
| `allowed_phones` | Whitelist of authorized remote employees |
| `unlock_requests` | History of AD account unlocks |
| `firewall_requests` | History of FortiGate session terminations |
| `human_requests` | Escalations handled by the human support team |

---

## 🔄 Automatic HR synchronization

To keep the whitelist up to date automatically, create a scheduled job that queries the remote employee list from your HR system and calls `BulkSyncAllowedPhones`. Example with cron:

```bash
# crontab -e  (runs nightly at 2am)
0 2 * * * /opt/support-bot/sync-whitelist >> /var/log/support-bot-sync.log 2>&1
```

For robust synchronization, compile a separate binary that:
1. Queries remote employees from your HR system or Active Directory
2. Builds a `[]db.AllowedPhone` slice
3. Calls `db.BulkSyncAllowedPhones(entries)` — which automatically deactivates employees removed from the list

---

## 🛡️ Security best practices

| Item | Recommendation |
|---|---|
| **Credentials** | Never commit `.env`. Use CI/CD secrets or a vault in production. |
| **AD TLS** | Prefer `ldaps://` (port 636). For full certificate validation, set `tls_skip_verify: false` and provide the CA. |
| **PostgreSQL** | Restrict database access to the bot host only via `pg_hba.conf`. |
| **FortiGate tokens** | Use API tokens with minimum permissions: Monitor (read) + Network (deauth only). |
| **Whitelist fail-closed** | Database errors during phone verification block access by default — they never grant it. |
| **`.env` file** | `chmod 600 .env` and `chown support-bot:support-bot .env`. |
| **WhatsApp session** | `wa-sessions.db` contains device private keys — never version or back up to an unencrypted location. |

---

## 🖼️ VPN guide images

FortiClient screenshots go in `vpn.images_path` (default: `/opt/support-bot/vpn-images`). Name the files as referenced in `vpn.go`:

| File | Expected content |
|---|---|
| `02-tela-inicial.png` | FortiClient home screen with the side panel |
| `03-perfil-vpn.png` | Connection profile configuration screen |
| `04-login-vpn.png` | Login screen (username and password) |
| `05-conectado.png` | "Connected" status with green icon |

If an image is not found, the bot sends the corresponding text message as a fallback.

---

## 🚧 In development

| Feature | Status |
|---|---|
| 🖥️ **Audit web panel** | In progress — web interface to view operation history, manage the phone whitelist, and monitor open support requests, with basic authentication configurable via `WEB_USERNAME` / `WEB_PASSWORD` in `.env` |

---

## 👤 Author

**Henry Victor Passold Gomes** — IT Infrastructure & Support Lead  
[LinkedIn](https://www.linkedin.com/in/henry-victor-passold-gomes) · [GitHub](https://github.com/imHenrybtw)

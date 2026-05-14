# 🤖 Bot de Suporte TI — WhatsApp

Bot de atendimento para equipes de TI que dá suporte a **funcionários em home office** diretamente pelo WhatsApp, sem depender de acesso à rede corporativa.

---

## 🎯 O problema que este projeto resolve

Funcionários em home office estão fora da rede interna. Quando enfrentam problemas de acesso — conta bloqueada no AD, sessão de firewall travada, VPN que não conecta — eles não conseguem abrir um chamado pelo portal interno nem acessar a intranet. O único canal que **sempre funciona** é o celular.

Sem este bot, o atendimento dessas situações depende de:

- Ligação para um ramal interno (inacessível de fora)
- E-mail, que exige que o usuário esteja logado (impossível se a conta está bloqueada)
- Acesso ao GLPI/ServiceDesk via VPN (sem VPN, não abre)

O resultado é o funcionário parado esperando alguém da TI responder manualmente.

Este bot elimina essa dependência: o usuário envia uma mensagem no WhatsApp e resolve o problema **sem intervenção humana** nos casos mais comuns.

---

## ✅ Funcionalidades

| Funcionalidade | Descrição |
|---|---|
| 🔓 **Desbloqueio de conta AD** | Localiza a conta no Active Directory, envia código de confirmação por e-mail e desbloqueia automaticamente |
| 🔥 **Derrubada de sessão de firewall** | Consulta todos os Fortinets configurados, localiza sessões ativas e as encerra via API |
| 🌐 **Guia de VPN passo a passo** | Envia tutorial ilustrado com capturas de tela do FortiClient |
| 🧑‍💼 **Escalonamento humano** | Abre chamado no GLPI, notifica a equipe no Teams e monitora SLA de resposta |
| ⏱️ **Timeouts automáticos** | Avisa o usuário se ninguém atendeu no prazo e encerra sessões inativas |
| 🔐 **Whitelist de acesso** | Apenas funcionários home office cadastrados podem interagir com o bot |

---

## 🔐 Segurança — Whitelist de telefones

O bot mantém uma tabela `allowed_phones` no PostgreSQL com os números autorizados. Mensagens de números não cadastrados são **descartadas silenciosamente**, sem resposta, para não confirmar a existência do serviço.

### Por que isso importa

- O número do WhatsApp do bot pode ser divulgado acidentalmente
- Sem whitelist, qualquer pessoa poderia acionar o fluxo de desbloqueio de contas ou derrubada de sessões
- A whitelist impede até mesmo o início do fluxo para usuários não autorizados

### Fluxo de autorização

```
Mensagem recebida
        │
        ▼
IsPhoneAllowed(phone)?  ──── NÃO ────▶  descarta silenciosamente (fail-closed)
        │
       SIM
        │
        ▼
bot.Handle(...)  ──▶  resposta normal
```

A verificação usa um índice parcial `WHERE active = TRUE`, garantindo lookup O(log n).

### Gerenciar a whitelist

**Adicionar manualmente via SQL:**
```sql
INSERT INTO allowed_phones (phone, matricula, name)
VALUES ('11991234567', 'JSILVA', 'João Silva')
ON CONFLICT (phone) DO UPDATE
    SET active = TRUE, name = EXCLUDED.name, updated_at = NOW();
```

**Desativar (soft-delete):**
```sql
UPDATE allowed_phones SET active = FALSE, updated_at = NOW()
WHERE phone = '11991234567';
```

**Via código Go (integração com RH):**
```go
// Adicionar individualmente
db.AddAllowedPhone("11991234567", "JSILVA", "João Silva")

// Sincronização em lote — substitui toda a lista atomicamente
entries := []db.AllowedPhone{
    {Phone: "11991234567", Matricula: "JSILVA", Name: "João Silva"},
    {Phone: "47987654321", Matricula: "MMATOS", Name: "Maria Matos"},
}
db.BulkSyncAllowedPhones(entries)
```

> **Dica:** Agende `BulkSyncAllowedPhones` para rodar diariamente, alimentado pelos funcionários em home office do seu sistema de RH ou de um grupo no AD (ex: `GRP_HomeOffice`).

---

## 🏗️ Arquitetura

```
WhatsApp (whatsmeow)
        │
        ▼
  eventHandler
        │
        ├── IsPhoneAllowed()  ◀── PostgreSQL: allowed_phones (whitelist)
        │
        ├── bot.Handle()      ◀── Máquina de estados por sessão
        │       │
        │       ├── AD (LDAP/LDAPS)          → desbloqueio de conta
        │       ├── Fortigate (REST API)     → sessões de firewall
        │       ├── GLPI (OAuth2 REST API)   → abertura de chamados
        │       ├── SMTP                     → códigos de confirmação
        │       └── Teams (Webhook)          → notificações à equipe
        │
        └── startCheckers()   → goroutine de timeouts (1 min)
```

---

## ⚙️ Instalação

### Pré-requisitos

- Go 1.22+
- PostgreSQL 14+
- Acesso de rede ao AD (LDAP/LDAPS), Fortigate e GLPI

### 1. Clonar e compilar

```bash
git clone https://github.com/imHenrybtw/remote-support-bot
cd support-bot
go build -o support-bot ./...
```

### 2. Configurar credenciais

```bash
cp .env.example .env
# Edite o .env e preencha todos os valores
```

### 3. Configurar o `config.yaml`

Ajuste os valores de acordo com seu ambiente. Os campos principais:

```yaml
bot:
  company_name: "Minha Empresa"   # aparece nos e-mails e cards do Teams

ad:
  host:    "ldaps://dc.empresa.local:636"
  bind_dn: "CN=ldap bot,OU=Servicos,DC=empresa,DC=local"
  base_dn: "DC=empresa,DC=local"

smtp:
  host: "192.168.1.10"
  from: "Bot Suporte TI <no-reply@empresa.com.br>"

firewalls:
  - name: "Matriz"              # FW_TOKEN_Matriz no .env
    host: "https://fortigate.empresa.com.br"

glpi:
  url:          "https://glpi.empresa.com.br/api.php/v2"
  ticket_title: "Suporte via WhatsApp"

vpn:
  gateway:      "ssl-vpn.empresa.com.br"
  port:         8443
  profile_name: "VPN-Empresa"
  images_path:  "/opt/support-bot/vpn-images"
```

> **VPN:** O tutorial enviado ao usuário usa `gateway`, `port` e `profile_name` diretamente do `config.yaml`. Basta alterar esses valores para adaptar ao seu ambiente — sem recompilar.

### 4. Criar o banco de dados

```bash
psql -U postgres -c "CREATE USER botsuporte WITH PASSWORD 'senha_aqui';"
psql -U postgres -c "CREATE DATABASE support_bot OWNER botsuporte;"
```

As tabelas são criadas automaticamente na primeira execução.

### 5. Popular a whitelist

```bash
psql -U botsuporte -d support_bot -c "
INSERT INTO allowed_phones (phone, matricula, name) VALUES
  ('11991234567', 'JSILVA', 'João Silva'),
  ('47987654321', 'MMATOS', 'Maria Matos');
"
```

### 6. Executar

```bash
./support-bot config.yaml
```

Na primeira execução (sem sessão salva), um QR Code aparece no terminal. Escaneie com o WhatsApp em **Aparelhos conectados → Conectar um aparelho**.

### 7. Instalar como serviço (systemd)

```bash
# Cria usuário dedicado (sem shell, sem home)
sudo useradd -r -s /sbin/nologin support-bot

# Copia os arquivos
sudo mkdir -p /opt/support-bot
sudo cp support-bot config.yaml .env /opt/support-bot/
sudo chown -R support-bot:support-bot /opt/support-bot
sudo chmod 600 /opt/support-bot/.env   # somente o dono lê as credenciais

# Instala e inicia o serviço
sudo cp support-bot.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now support-bot

# Acompanhar logs em tempo real
sudo journalctl -u support-bot -f
```

---

## 📁 Estrutura do projeto

```
.
├── main.go               # Entrada, handler do WhatsApp, whitelist guard
├── config.go             # Leitura de config.yaml + .env
├── config.yaml           # Configuração (seguro para commitar)
├── .env.example          # Modelo de credenciais (nunca commitar o .env)
├── .gitignore
├── db.go                 # PostgreSQL: migrations, CRUD, whitelist
├── ad.go                 # Cliente LDAP/LDAPS para o Active Directory
├── fortigate.go          # API REST do Fortigate (sessões e deauth)
├── glpi.go               # API REST do GLPI com OAuth2
├── mail.go               # Envio de e-mails transacionais (SMTP)
├── teams.go              # Notificações no Microsoft Teams (Adaptive Cards)
├── vpn.go                # Guia de VPN com imagens (configurável via YAML)
└── support-bot.service   # Unit file do systemd
```

---

## 🗄️ Schema do banco de dados

| Tabela | Propósito |
|---|---|
| `allowed_phones` | Whitelist de funcionários home office autorizados |
| `unlock_requests` | Histórico de desbloqueios de conta no AD |
| `firewall_requests` | Histórico de derrubadas de sessão no Fortigate |
| `human_requests` | Atendimentos escalados para a equipe humana |

---

## 🔄 Sincronização automática com o RH

Para manter a whitelist atualizada automaticamente, crie um job agendado que consulte a base de funcionários em home office e chame `BulkSyncAllowedPhones`. Exemplo com cron:

```bash
# crontab -e  (executa toda noite às 2h)
0 2 * * * /opt/support-bot/sync-whitelist >> /var/log/support-bot-sync.log 2>&1
```

Para sincronização robusta, compile um binário separado que:
1. Consulta os funcionários em home office no seu sistema de RH ou AD
2. Monta um slice `[]db.AllowedPhone`
3. Chama `db.BulkSyncAllowedPhones(entries)` — que desativa automaticamente quem saiu da lista

---

## 🛡️ Boas práticas de segurança

| Item | Recomendação |
|---|---|
| **Credenciais** | Nunca commite o `.env`. Use segredos do CI/CD ou um vault em produção. |
| **TLS do AD** | Prefira `ldaps://` (porta 636). Para validação completa, defina `tls_skip_verify: false` e forneça o CA. |
| **PostgreSQL** | Restrinja o acesso ao banco apenas ao host do bot via `pg_hba.conf`. |
| **Tokens Fortigate** | Use tokens de API com permissões mínimas: Monitor (leitura) + Network (deauth). |
| **Whitelist fail-closed** | Erros de banco na verificação do telefone bloqueiam o acesso por padrão — nunca liberam. |
| **Arquivo .env** | `chmod 600 .env` e `chown support-bot:support-bot .env`. |
| **Sessão WhatsApp** | `wa-sessions.db` contém chaves privadas do dispositivo — nunca versionar nem fazer backup em local não criptografado. |

---

## 🖼️ Imagens do guia de VPN

As capturas de tela do FortiClient ficam em `vpn.images_path` (padrão: `/opt/support-bot/vpn-images`). Nomeie os arquivos conforme referenciado em `vpn.go`:

| Arquivo | Conteúdo esperado |
|---|---|
| `02-tela-inicial.png` | Tela inicial do FortiClient com o painel lateral |
| `03-perfil-vpn.png` | Tela de configuração do perfil de conexão |
| `04-login-vpn.png` | Tela de login (usuário e senha) |
| `05-conectado.png` | Status "Conectado" com ícone verde |

Se uma imagem não for encontrada, o bot envia a mensagem de texto correspondente como fallback.

## 🚧 Em desenvolvimento
 
| Funcionalidade | Status |
|---|---|
| 🖥️ **Painel web de auditoria** | Em desenvolvimento — interface para visualizar histórico de operações, gerenciar a whitelist de telefones e acompanhar atendimentos em aberto, com autenticação básica configurável via `WEB_USERNAME` / `WEB_PASSWORD` no `.env` |


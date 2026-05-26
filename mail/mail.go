package mail

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"net/smtp"
	"strings"
	"time"

	"whatsapp-bot/config"
)

// ════════════════════════════════════════════════════════════════════════════
//
//  Envio de e-mails transacionais do bot.
//
//  Cada mensagem é enviada como multipart/alternative com duas partes:
//
//    1. text/plain — fallback para clientes/leitores que não renderizam HTML
//    2. text/html  — versão visual com layout e destaque do código
//
//  CSS é todo inline porque clientes de e-mail (especialmente o Outlook) não
//  honram <style> nem várias regras modernas.
//
// ════════════════════════════════════════════════════════════════════════════

const smtpTimeout = 15 * time.Second

// ─── Envio bruto (multipart) ────────────────────────────────────────────────

func send(to, subject, plainBody, htmlBody string) error {
	cfg := config.C.SMTP

	// Sanitiza destinatário — previne injeção de cabeçalhos SMTP via CRLF.
	to = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, strings.TrimSpace(to))
	if to == "" {
		return fmt.Errorf("destinatário vazio")
	}

	boundary := "boundary_" + mustRandHex(8)
	encSubject := "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(subject)) + "?="

	var msg strings.Builder
	w := func(s string) { msg.WriteString(s); msg.WriteString("\r\n") }

	w("From: " + cfg.From)
	w("To: " + to)
	w("Subject: " + encSubject)
	w("MIME-Version: 1.0")
	w("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"")
	w("")
	w("Esta mensagem está em formato multipart MIME.")
	w("")

	w("--" + boundary)
	w("Content-Type: text/plain; charset=UTF-8")
	w("Content-Transfer-Encoding: 8bit")
	w("")
	msg.WriteString(plainBody)
	w("")

	w("--" + boundary)
	w("Content-Type: text/html; charset=UTF-8")
	w("Content-Transfer-Encoding: 8bit")
	w("")
	msg.WriteString(htmlBody)
	w("")
	w("--" + boundary + "--")

	addr := net.JoinHostPort(host, strconv.Itoa(port))

	// Usa net.Dial com timeout explícito — smtp.SendMail não suporta contexto.
	conn, err := net.DialTimeout("tcp", addr, smtpTimeout)
	if err != nil {
		return fmt.Errorf("SMTP connect: %w", err)
	}
	conn.SetDeadline(time.Now().Add(smtpTimeout)) //nolint:errcheck

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("SMTP client: %w", err)
	}
	defer client.Close()

	if cfg.User != "" {
		auth := smtp.PlainAuth("", cfg.User, cfg.Password, cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}

	from := extractAddr(cfg.From)
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("SMTP MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("SMTP RCPT TO: %w", err)
	}

	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	if _, err := wc.Write([]byte(msg.String())); err != nil {
		wc.Close()
		return fmt.Errorf("SMTP write: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("SMTP data close: %w", err)
	}
	return client.Quit()
}

func extractAddr(from string) string {
	if i := strings.Index(from, "<"); i >= 0 {
		return strings.Trim(from[i:], "<>")
	}
	return from
}

// mustRandHex gera nBytes de entropia aleatória como hex. Faz panic em falha
// de sistema (crypto/rand não deve falhar em ambientes normais).
func mustRandHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand falhou: %v", err))
	}
	return hex.EncodeToString(b)
}

// ─── Template HTML base ─────────────────────────────────────────────────────

func htmlShell(preheader, headline, innerHTML string) string {
	companyName := config.C.Bot.CompanyName
	if companyName == "" {
		companyName = "Suporte TI"
	}

	return fmt.Sprintf(`<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd">
<html xmlns="http://www.w3.org/1999/xhtml">
<head>
<meta http-equiv="Content-Type" content="text/html; charset=UTF-8" />
<meta name="viewport" content="width=device-width, initial-scale=1.0" />
<title>%s</title>
</head>
<body style="margin:0;padding:0;background:#f4f5f7;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;">

<div style="display:none;max-height:0;overflow:hidden;">%s</div>

<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" border="0" style="background:#f4f5f7;padding:32px 16px;">
  <tr>
    <td align="center">
      <table role="presentation" width="600" cellpadding="0" cellspacing="0" border="0"
             style="background:#ffffff;border-radius:10px;overflow:hidden;box-shadow:0 1px 3px rgba(15,23,42,0.06);max-width:600px;width:100%%;">

        <tr>
          <td style="background:#0f172a;padding:24px 28px;">
            <table role="presentation" width="100%%" cellpadding="0" cellspacing="0" border="0">
              <tr>
                <td style="color:#ffffff;font-size:18px;font-weight:600;line-height:1.3;">
                  🤖&nbsp;&nbsp;Bot de Suporte TI
                </td>
                <td align="right" style="color:#94a3b8;font-size:12px;">%s</td>
              </tr>
            </table>
          </td>
        </tr>

        <tr>
          <td style="padding:32px 28px 8px 28px;">
            <h1 style="margin:0 0 16px 0;color:#0f172a;font-size:20px;font-weight:600;line-height:1.3;">
              %s
            </h1>
            <div style="color:#334155;font-size:14px;line-height:1.6;">
              %s
            </div>
          </td>
        </tr>

        <tr>
          <td style="background:#f8fafc;border-top:1px solid #e2e8f0;padding:18px 28px;
                     color:#64748b;font-size:12px;line-height:1.5;text-align:center;">
            Mensagem automática · Não responda a este e-mail.<br/>
            Se não foi você quem solicitou, pode ignorar com segurança.
          </td>
        </tr>

      </table>
    </td>
  </tr>
</table>
</body>
</html>`, headline, preheader, companyName, headline, innerHTML)
}

func codeBlock(code string) string {
	return fmt.Sprintf(`<div style="text-align:center;margin:28px 0;">
  <span style="display:inline-block;background:#dbeafe;color:#1e3a8a;
               padding:18px 36px;border-radius:10px;
               font-family:'SF Mono','Consolas','Courier New',monospace;
               font-size:30px;letter-spacing:8px;font-weight:700;">%s</span>
</div>`, code)
}

func dataTable(rows ...[2]string) string {
	var b strings.Builder
	b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"
                  style="background:#f8fafc;border:1px solid #e2e8f0;border-radius:8px;
                         margin:16px 0;font-size:14px;">`)
	for i, r := range rows {
		border := "border-top:1px solid #e2e8f0;"
		if i == 0 {
			border = ""
		}
		b.WriteString(fmt.Sprintf(`<tr>
  <td style="padding:10px 14px;color:#64748b;width:35%%;%s">%s</td>
  <td style="padding:10px 14px;color:#0f172a;font-weight:600;%s">%s</td>
</tr>`, border, r[0], border, r[1]))
	}
	b.WriteString(`</table>`)
	return b.String()
}

// ─── E-mails específicos ────────────────────────────────────────────────────

func SendUnlockCode(toEmail, firstName, targetName, targetMatricula, code string) error {
	subject := "[Bot Suporte TI] Código de confirmação — Desbloqueio de conta"

	plain := fmt.Sprintf(`Olá, %s!

Você solicitou o desbloqueio da seguinte conta via WhatsApp:

  Conta    : %s
  Matrícula: %s

Informe o código abaixo na conversa do WhatsApp para confirmar:

  ┌─────────────┐
  │   %s    │
  └─────────────┘

⏱️  Expira em 10 minutos.
Se não foi você, ignore este e-mail.

— Bot de Suporte TI
`, firstName, targetName, targetMatricula, code)

	inner := fmt.Sprintf(`
<p style="margin:0 0 12px 0;">Olá, <strong>%s</strong>!</p>
<p style="margin:0 0 12px 0;">Você solicitou via WhatsApp o desbloqueio da conta abaixo:</p>

%s

<p style="margin:24px 0 0 0;">Informe o código a seguir na conversa do WhatsApp para confirmar:</p>

%s

<p style="margin:0;color:#64748b;font-size:13px;">
  ⏱&nbsp; Expira em <strong>10 minutos</strong>.
</p>
`,
		firstName,
		dataTable(
			[2]string{"👤 Conta", targetName},
			[2]string{"🪪 Matrícula", targetMatricula},
		),
		codeBlock(code),
	)

	html := htmlShell("Código de confirmação para desbloqueio de conta", "Confirme o desbloqueio da conta", inner)
	return send(toEmail, subject, plain, html)
}

func SendFirewallCode(toEmail, firstName, targetMatricula string, sessionsFound int, code string) error {
	subject := "[Bot Suporte TI] Código de confirmação — Derrubada de sessão"

	plain := fmt.Sprintf(`Olá, %s!

Você solicitou via WhatsApp a derrubada de sessões de firewall para:

  Matrícula                : %s
  Sessões ativas encontradas: %d

Informe o código abaixo na conversa do WhatsApp para confirmar:

  ┌─────────────┐
  │   %s    │
  └─────────────┘

⏱️  Expira em 10 minutos.
Se não foi você, ignore este e-mail.

— Bot de Suporte TI
`, firstName, targetMatricula, sessionsFound, code)

	inner := fmt.Sprintf(`
<p style="margin:0 0 12px 0;">Olá, <strong>%s</strong>!</p>
<p style="margin:0 0 12px 0;">Você solicitou via WhatsApp a derrubada de sessões de firewall do usuário:</p>

%s

<p style="margin:24px 0 0 0;">Informe o código a seguir na conversa do WhatsApp para confirmar:</p>

%s

<p style="margin:0;color:#64748b;font-size:13px;">
  ⏱&nbsp; Expira em <strong>10 minutos</strong>.
</p>
`,
		firstName,
		dataTable(
			[2]string{"🪪 Matrícula", targetMatricula},
			[2]string{"🔥 Sessões ativas", fmt.Sprintf("%d", sessionsFound)},
		),
		codeBlock(code),
	)

	html := htmlShell("Código de confirmação para derrubada de sessão de firewall", "Confirme a derrubada de sessões", inner)
	return send(toEmail, subject, plain, html)
}

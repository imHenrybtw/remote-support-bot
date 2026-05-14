package teams

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"whatsapp-bot/config"
	"whatsapp-bot/glpi"
)

// ════════════════════════════════════════════════════════════════════════════
//
//  Notificações no Microsoft Teams via webhook (Incoming Webhook ou Workflow).
//
//  Os cards usam Adaptive Cards 1.4. A escolha de elementos foi pensada para
//  que apareça bem tanto no Teams Desktop quanto no Mobile:
//
//    • Container com style:"attention"/"warning" + bleed:true para um header
//      colorido full-width.
//    • FactSet para chave-valor (alinhamento automático, mais limpo que
//      múltiplos TextBlocks com markdown bold).
//    • Separadores horizontais entre as seções.
//    • Action.OpenUrl com style:"positive" no botão principal.
//
// ════════════════════════════════════════════════════════════════════════════

// ─── HTTP ────────────────────────────────────────────────────────────────────

var httpClient = &http.Client{Timeout: 15 * time.Second}

func post(payload map[string]interface{}) error {
	url := config.C.Bot.TeamsWebhookURL
	if url == "" {
		fmt.Println("[TEAMS] Webhook URL não configurada — pulando notificação.")
		return nil
	}

	body, _ := json.Marshal(payload)
	resp, err := httpClient.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("erro ao enviar para Teams: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Teams retornou HTTP %d", resp.StatusCode)
	}
	return nil
}

// ─── Builders de elementos ──────────────────────────────────────────────────

// banner cria o "header" colorido do card. Aceita "attention" (vermelho),
// "warning" (laranja), "good" (verde) ou "emphasis" (cinza).
func banner(title, style string) map[string]interface{} {
	return map[string]interface{}{
		"type":  "Container",
		"style": style,
		"bleed": true,
		"items": []map[string]interface{}{
			{
				"type":   "TextBlock",
				"text":   title,
				"weight": "Bolder",
				"size":   "Medium",
				"color":  "Light",
				"wrap":   true,
			},
		},
	}
}

// factSet monta uma tabela de chave-valor.
func factSet(facts ...[2]string) map[string]interface{} {
	items := make([]map[string]string, 0, len(facts))
	for _, f := range facts {
		items = append(items, map[string]string{
			"title": f[0],
			"value": f[1],
		})
	}
	return map[string]interface{}{
		"type":  "FactSet",
		"facts": items,
	}
}

// sectionTitle é o pequeno título de uma seção dentro do card.
func sectionTitle(text string) map[string]interface{} {
	return map[string]interface{}{
		"type":      "TextBlock",
		"text":      text,
		"weight":    "Bolder",
		"size":      "Small",
		"color":     "Accent",
		"spacing":   "Medium",
		"separator": true,
	}
}

// paragraph é um bloco de texto longo, com wrap.
func paragraph(text string) map[string]interface{} {
	return map[string]interface{}{
		"type":    "TextBlock",
		"text":    text,
		"wrap":    true,
		"spacing": "Small",
	}
}

// timestamp em "DD/MM/YYYY HH:MM" no fuso local.
func timestamp() string {
	return time.Now().Format("02/01/2006 15:04")
}

// ─── Card builder ───────────────────────────────────────────────────────────

func adaptiveCard(body []map[string]interface{}, actions []map[string]interface{}) map[string]interface{} {
	if actions == nil {
		actions = []map[string]interface{}{}
	}
	return map[string]interface{}{
		"type": "message",
		"attachments": []map[string]interface{}{
			{
				"contentType": "application/vnd.microsoft.card.adaptive",
				"content": map[string]interface{}{
					"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
					"type":    "AdaptiveCard",
					"version": "1.4",
					"body":    body,
					"actions": actions,
				},
			},
		},
	}
}

// glpiTicketURL retorna a URL do chamado no painel do GLPI.
func glpiTicketURL(ticketID int) string {
	return glpi.TicketURL(ticketID)
}

// ─── Notificações públicas ──────────────────────────────────────────────────

// SendAlert envia ao Teams um card de "novo chamado aberto via WhatsApp".
func SendAlert(userName, matricula, phone, description string, ticketID int) {
	chamadoLabel := "Em processamento"
	if ticketID > 0 {
		chamadoLabel = fmt.Sprintf("#%d", ticketID)
	}

	body := []map[string]interface{}{
		banner("🚨  Novo chamado via WhatsApp", "attention"),

		factSet(
			[2]string{"👤  Usuário", fmt.Sprintf("%s  *(%s)*", userName, matricula)},
			[2]string{"📱  WhatsApp", phone},
			[2]string{"🎫  Chamado", chamadoLabel},
			[2]string{"🕒  Aberto em", timestamp()},
		),

		sectionTitle("📝  PROBLEMA RELATADO"),
		paragraph(description),
	}

	var actions []map[string]interface{}
	if ticketID > 0 {
		actions = []map[string]interface{}{
			{
				"type":  "Action.OpenUrl",
				"title": "Abrir chamado no GLPI",
				"url":   glpiTicketURL(ticketID),
				"style": "positive",
			},
		}
	}

	if err := post(adaptiveCard(body, actions)); err != nil {
		fmt.Printf("[TEAMS] SendAlert erro: %v\n", err)
	}
}

// SendWaitingAlert notifica o Teams que um usuário está aguardando
// atendimento há mais de waitMinutes minutos sem resposta.
func SendWaitingAlert(userName, matricula, phone string, ticketID, waitMinutes int) {
	chamadoLabel := "Em processamento"
	if ticketID > 0 {
		chamadoLabel = fmt.Sprintf("#%d", ticketID)
	}

	body := []map[string]interface{}{
		banner("⏳  Usuário aguardando atendimento", "warning"),

		paragraph(fmt.Sprintf(
			"Nenhum atendente respondeu nos últimos **%d minutos**. Verifique o chamado.",
			waitMinutes,
		)),

		factSet(
			[2]string{"👤  Usuário", fmt.Sprintf("%s  *(%s)*", userName, matricula)},
			[2]string{"📱  WhatsApp", phone},
			[2]string{"🎫  Chamado", chamadoLabel},
			[2]string{"🕒  Aguardando desde", timestamp()},
		),
	}

	var actions []map[string]interface{}
	if ticketID > 0 {
		actions = []map[string]interface{}{
			{
				"type":  "Action.OpenUrl",
				"title": "Abrir chamado no GLPI",
				"url":   glpiTicketURL(ticketID),
				"style": "positive",
			},
		}
	}

	if err := post(adaptiveCard(body, actions)); err != nil {
		fmt.Printf("[TEAMS] SendWaitingAlert erro: %v\n", err)
	}
}

// SendFollowUpAlert notifica o Teams com urgência que o usuário informou que
// o problema NÃO foi resolvido após o atendimento.
func SendFollowUpAlert(userName, matricula, phone string, ticketID int) {
	chamadoLabel := "Em processamento"
	if ticketID > 0 {
		chamadoLabel = fmt.Sprintf("#%d", ticketID)
	}

	body := []map[string]interface{}{
		banner("🚨  URGENTE — Usuário não foi atendido", "attention"),

		paragraph(
			"O usuário informou via WhatsApp que **o problema não foi resolvido** " +
				"após o atendimento. Ação imediata necessária.",
		),

		factSet(
			[2]string{"👤  Usuário", fmt.Sprintf("%s  *(%s)*", userName, matricula)},
			[2]string{"📱  WhatsApp", phone},
			[2]string{"🎫  Chamado", chamadoLabel},
			[2]string{"🕒  Reportado em", timestamp()},
		),
	}

	var actions []map[string]interface{}
	if ticketID > 0 {
		actions = []map[string]interface{}{
			{
				"type":  "Action.OpenUrl",
				"title": "Verificar chamado no GLPI",
				"url":   glpiTicketURL(ticketID),
				"style": "positive",
			},
		}
	}

	card := adaptiveCard(body, actions)
	// Sinaliza urgência no Teams (notificação prioritária).
	if attachments, ok := card["attachments"].([]map[string]interface{}); ok && len(attachments) > 0 {
		if content, ok := attachments[0]["content"].(map[string]interface{}); ok {
			content["msteams"] = map[string]interface{}{
				"importance": "urgent",
			}
		}
	}

	if err := post(card); err != nil {
		fmt.Printf("[TEAMS] SendFollowUpAlert erro: %v\n", err)
	}
}

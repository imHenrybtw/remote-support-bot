package glpi

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"whatsapp-bot/config"
)

// ════════════════════════════════════════════════════════════════════════════
//
//  Cliente GLPI — API REST com suporte a OAuth2 (GLPI 11+) e tokens legados.
//
//  Notas de implementação:
//
//  • Endpoint de criação: /api.php/Assistance/Ticket (sem prefixo de versão).
//    A doc oficial afirma que, sem versão explícita, a "latest" é usada.
//
//  • Alguns deployments de GLPI retornam HTTP 404 com mensagem de erro
//    mesmo quando o ticket é criado com sucesso. Isso é efeito colateral
//    de plugins de numeração customizada. Por isso NÃO confiamos no status
//    do POST: embutimos um tracking UUID no content e em seguida fazemos um
//    GET com filtro RSQL para obter o ID real do ticket.
//
//  • O filtro RSQL é preciso por campo: content=ilike=*X* só varre content.
//    O tracking UUID vai apenas na descrição para manter o título limpo.
//
// ════════════════════════════════════════════════════════════════════════════

const (
	pathToken  = "/api.php/token"
	pathTicket = "/api.php/Assistance/Ticket"

	// Espera antes de buscar o ticket recém-criado (tempo de indexação).
	// Aumente se o GLPI estiver muito carregado.
	indexDelay = 1500 * time.Millisecond

	// Tentativas e intervalo entre elas na busca pós-criação.
	searchAttempts = 3
	searchBackoff  = 1 * time.Second

	// Prefixo do tracking ID — facilita identificar chamados do bot no painel.
	trackPrefix = "wabot-"
)

var (
	httpClient = &http.Client{Timeout: 30 * time.Second}

	tokenMu     sync.Mutex
	cachedToken string
	tokenExp    time.Time
)

// ─── Helpers de URL ──────────────────────────────────────────────────────────

// baseURL retorna a raiz do GLPI, removendo sufixos de versão que possam
// estar em config.C.GLPI.URL.
func baseURL() string {
	u := strings.TrimRight(config.C.GLPI.URL, "/")
	u = strings.TrimSuffix(u, "/api.php/v2")
	u = strings.TrimSuffix(u, "/apirest.php")
	u = strings.TrimSuffix(u, "/api.php")
	return strings.TrimRight(u, "/")
}

// ─── OAuth2 ──────────────────────────────────────────────────────────────────

// getToken devolve o access_token, renovando-o se faltar menos de 1 minuto
// para expirar. Thread-safe.
func getToken() (string, error) {
	tokenMu.Lock()
	defer tokenMu.Unlock()

	if cachedToken != "" && time.Now().Before(tokenExp.Add(-1*time.Minute)) {
		return cachedToken, nil
	}

	cfg := config.C.GLPI
	payload := map[string]string{
		"grant_type":    "password",
		"client_id":     cfg.OAuthClientID,
		"client_secret": cfg.OAuthClientSecret,
		"username":      cfg.OAuthUsername,
		"password":      cfg.OAuthPassword,
		"scope":         "api",
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", baseURL()+pathToken, bytes.NewBuffer(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("OAuth: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OAuth HTTP %d: %s",
			resp.StatusCode, truncate(string(respBody), 300))
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &tr); err != nil {
		return "", fmt.Errorf("resposta OAuth inválida: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("access_token vazio")
	}

	cachedToken = tr.AccessToken
	tokenExp = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return cachedToken, nil
}

// ─── API pública ─────────────────────────────────────────────────────────────

// OpenTicket cria um chamado no GLPI e retorna o ID.
//
// O título do chamado vem de config.yaml → glpi.ticket_title.
// A identificação do usuário (telefone + matrícula) e a descrição do
// problema são embutidas no corpo do ticket.
func OpenTicket(phone, matricula, description string) (int, error) {
	cfg := config.C.GLPI
	if cfg.OAuthClientID == "" || cfg.OAuthClientSecret == "" ||
		cfg.OAuthUsername == "" || cfg.OAuthPassword == "" {
		return 0, fmt.Errorf("GLPI: credenciais OAuth não configuradas")
	}

	token, err := getToken()
	if err != nil {
		return 0, err
	}

	// Tracking UUID — vai apenas na descrição; nunca no título.
	track := trackPrefix + randomHex(8)

	name := cfg.TicketTitle
	if name == "" {
		name = "Suporte via WhatsApp"
	}

	content := fmt.Sprintf(
		"Chamado aberto via WhatsApp Bot.\n\n"+
			"📱 Celular  : %s\n"+
			"🪪 Matrícula: %s\n\n"+
			"📝 Descrição:\n%s\n\n"+
			"---\n[track-id: %s]",
		phone, matricula, description, track,
	)

	if err := postTicket(token, name, content); err != nil {
		return 0, fmt.Errorf("POST Ticket: %w", err)
	}

	// Aguarda indexação antes de buscar o ticket pelo tracking ID.
	time.Sleep(indexDelay)

	id, err := findByTrack(token, track)
	if err != nil {
		// Registra o tracking para recuperação manual no painel se necessário.
		fmt.Printf("[GLPI] aviso: ticket criado mas não localizado. "+
			"Busque no painel por: %s\n", track)
		return 0, fmt.Errorf("ticket criado mas não localizado (track=%s): %w",
			track, err)
	}
	return id, nil
}

// TicketURL retorna a URL do chamado no painel web do GLPI.
func TicketURL(ticketID int) string {
	return fmt.Sprintf("%s/index.php?redirect=ticket_%d", baseURL(), ticketID)
}

// SatisfactionLink retorna o link direto para o usuário avaliar o chamado
// no portal GLPI.
func SatisfactionLink(ticketID int) string {
	return fmt.Sprintf("%s/index.php?redirect=ticket_%d&forcetab=Ticket.Satisfaction.1",
		baseURL(), ticketID)
}

// ─── Implementação ──────────────────────────────────────────────────────────

// postTicket dispara o POST de criação. O status HTTP é intencionalmente
// ignorado: alguns deployments de GLPI retornam 404 mesmo quando a criação
// é bem-sucedida (efeito colateral de plugins de numeração customizada).
// Falha apenas se a request não conseguir sair do cliente (timeout, rede).
func postTicket(token, name, content string) error {
	cfg := config.C.GLPI

	payload := map[string]interface{}{
		"name":    name,
		"content": content,
		"type":    1, // 1 = Incidente, 2 = Requisição
		"entity":  0,
	}
	if cfg.CategoryID > 0 {
		payload["category"] = cfg.CategoryID
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", baseURL()+pathTicket, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return nil
}

// findByTrack localiza o ticket pelo tracking UUID via filtro RSQL.
// Tenta searchAttempts vezes com searchBackoff entre elas para tolerar
// atraso de indexação do GLPI.
func findByTrack(token, track string) (int, error) {
	rsql := fmt.Sprintf("content=ilike=*%s*", track)
	endpoint := fmt.Sprintf("%s%s?filter=%s&range=0-0",
		baseURL(), pathTicket, url.QueryEscape(rsql))

	var lastErr error
	for attempt := 1; attempt <= searchAttempts; attempt++ {
		req, _ := http.NewRequest("GET", endpoint, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(searchBackoff)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			lastErr = fmt.Errorf("HTTP %d: %s",
				resp.StatusCode, truncate(string(body), 200))
			time.Sleep(searchBackoff)
			continue
		}

		var arr []map[string]interface{}
		if err := json.Unmarshal(body, &arr); err != nil {
			lastErr = fmt.Errorf("JSON inválido: %w", err)
			time.Sleep(searchBackoff)
			continue
		}
		if len(arr) > 0 {
			if v, ok := arr[0]["id"]; ok {
				switch x := v.(type) {
				case float64:
					return int(x), nil
				case string:
					var n int
					fmt.Sscanf(x, "%d", &n)
					if n > 0 {
						return n, nil
					}
				}
			}
		}
		lastErr = fmt.Errorf("nenhum match para %s", track)
		time.Sleep(searchBackoff)
	}
	return 0, fmt.Errorf("após %d tentativas: %w", searchAttempts, lastErr)
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

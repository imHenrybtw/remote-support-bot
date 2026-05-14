package fortigate

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"whatsapp-bot/config"
)

// UserSession representa uma sessão ativa de um usuário num firewall.
// O token do firewall nunca é armazenado aqui — é lido do config em tempo de uso.
type UserSession struct {
	Firewall     string // nome do firewall em config.yaml (ex: "Matriz")
	FirewallHost string
	IPAddr       string
	Username     string
	Method       string
}

// DeauthResult resume o resultado da desautenticação.
type DeauthResult struct {
	Total   int
	Success int
	Errors  int
}

func makeHTTPClient(skipVerify bool) *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: skipVerify}, //nolint:gosec
		},
	}
}

// GetUserSessions consulta todos os firewalls configurados e retorna
// as sessões ativas do usuário com a matrícula informada.
func GetUserSessions(matricula string) ([]UserSession, error) {
	var allSessions []UserSession
	matricula = strings.TrimSpace(strings.ToLower(matricula))

	for _, fw := range config.C.Firewalls {
		if fw.Token == "" {
			fmt.Printf("[FORTIGATE] Token não configurado para %s — pulando.\n", fw.Name)
			continue
		}
		sessions, err := getUserSessionsFromFirewall(fw, matricula)
		if err != nil {
			fmt.Printf("[FORTIGATE] Erro em %s: %v\n", fw.Name, err)
			continue // tenta o próximo firewall mesmo se um falhar
		}
		allSessions = append(allSessions, sessions...)
	}

	return allSessions, nil
}

func getUserSessionsFromFirewall(fw config.FirewallAPI, matricula string) ([]UserSession, error) {
	url := fmt.Sprintf("%s/api/v2/monitor/user/firewall?vdom=root", fw.Host)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+fw.Token)
	req.Header.Set("Accept", "application/json")

	client := makeHTTPClient(fw.TLSSkipVerify)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("conexão falhou: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return nil, fmt.Errorf("HTTP %d ao consultar sessões", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB max
	if err != nil {
		return nil, fmt.Errorf("falha ao ler resposta: %w", err)
	}

	var result struct {
		Results []struct {
			IPAddr   string `json:"ipaddr"`
			Username string `json:"username"`
			Method   string `json:"method"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("resposta inválida: %w", err)
	}

	var sessions []UserSession
	for _, r := range result.Results {
		if strings.EqualFold(r.Username, matricula) {
			sessions = append(sessions, UserSession{
				Firewall:     fw.Name,
				FirewallHost: fw.Host,
				IPAddr:       r.IPAddr,
				Username:     r.Username,
				Method:       r.Method,
			})
		}
	}
	return sessions, nil
}

// DeauthSessions desautentica todas as sessões fornecidas.
func DeauthSessions(sessions []UserSession) DeauthResult {
	result := DeauthResult{Total: len(sessions)}

	for _, s := range sessions {
		if err := deauthOne(s); err != nil {
			fmt.Printf("[FORTIGATE] Erro ao desautenticar %s em %s: %v\n", s.Username, s.Firewall, err)
			result.Errors++
		} else {
			result.Success++
		}
	}
	return result
}

func deauthOne(s UserSession) error {
	// O token é lido do config na hora do uso — nunca armazenado na sessão.
	token := ""
	for _, fw := range config.C.Firewalls {
		if fw.Name == s.Firewall {
			token = fw.Token
			break
		}
	}
	if token == "" {
		return fmt.Errorf("token não encontrado para firewall %q", s.Firewall)
	}

	url := fmt.Sprintf("%s/api/v2/monitor/user/firewall/deauth?vdom=root", s.FirewallHost)

	payload := map[string]interface{}{
		"ip":         s.IPAddr,
		"user_type":  "firewall",
		"id":         0,
		"ip_version": "ip4",
		"method":     s.Method,
		"username":   s.Username,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// Reutiliza a config TLSSkipVerify do firewall correspondente
	skipVerify := false
	for _, fw := range config.C.Firewalls {
		if fw.Name == s.Firewall {
			skipVerify = fw.TLSSkipVerify
			break
		}
	}

	client := makeHTTPClient(skipVerify)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d ao desautenticar", resp.StatusCode)
	}
	return nil
}

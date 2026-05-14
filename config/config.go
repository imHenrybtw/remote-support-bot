package config

import (
	"log"
	"os"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Bot       BotConfig      `yaml:"bot"`
	AD        ADConfig       `yaml:"ad"`
	SMTP      SMTPConfig     `yaml:"smtp"`
	Firewalls []FirewallAPI  `yaml:"firewalls"`
	GLPI      GLPIConfig     `yaml:"glpi"`
	Postgres  PostgresConfig `yaml:"postgres"`
	VPN       VPNConfig      `yaml:"vpn"`
	Web       WebConfig      `yaml:"web"`
}

type BotConfig struct {
	CompanyName              string   `yaml:"company_name"`
	SupportName              string   `yaml:"support_name"` // nome do time exibido no menu de boas-vindas
	Timezone                 string   `yaml:"timezone"`     // ex: "America/Sao_Paulo"
	WorkingHoursStart        string   `yaml:"working_hours_start"`
	WorkingHoursEnd          string   `yaml:"working_hours_end"`
	SaturdayHoursEnd         string   `yaml:"saturday_hours_end"`
	WorkingDays              []int    `yaml:"working_days"`
	UrgencyPhone             []string `yaml:"urgency_phone"`
	HumanTimeoutMinutes      int      `yaml:"human_timeout_minutes"`
	AttendantTimeoutMinutes  int      `yaml:"attendant_timeout_minutes"`
	InactivityTimeoutMinutes int      `yaml:"inactivity_timeout_minutes"`
	TeamsWebhookURL          string   `yaml:"teams_webhook_url"`
}

type ADConfig struct {
	Host          string   `yaml:"host"` // ldap:// ou ldaps://
	BindDN        string   `yaml:"bind_dn"`
	BindPassword  string   `yaml:"bind_password"`
	BaseDN        string   `yaml:"base_dn"`
	GroupUnlock   []string `yaml:"group_unlock"`
	UseStartTLS   bool     `yaml:"use_starttls"`    // STARTTLS em ldap:// (porta 389)
	TLSSkipVerify bool     `yaml:"tls_skip_verify"` // true = aceita cert self-signed
	TLSServerName string   `yaml:"tls_server_name"` // hostname do DC para validação TLS
}

type SMTPConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	From     string `yaml:"from"`
}

type FirewallAPI struct {
	Name          string `yaml:"name"`
	Host          string `yaml:"host"`
	TLSSkipVerify bool   `yaml:"tls_skip_verify"`
	Token         string `yaml:"-"` // preenchido em runtime via .env (FW_TOKEN_<name>)
}

type GLPIConfig struct {
	URL         string `yaml:"url"`
	CategoryID  int    `yaml:"category_id"`
	TicketTitle string `yaml:"ticket_title"` // título padrão dos chamados abertos pelo bot

	// Legacy API REST (GLPI 10 e anteriores)
	AppToken  string `yaml:"app_token"`
	UserToken string `yaml:"user_token"`

	// OAuth2 (GLPI 11+) — preenchidos via .env, nunca pelo YAML
	OAuthClientID     string `yaml:"-"`
	OAuthClientSecret string `yaml:"-"`
	OAuthUsername     string `yaml:"-"`
	OAuthPassword     string `yaml:"-"`

	// Usado internamente por outros pacotes (ex: teams.go para gerar URLs)
	TechnicianGroupID int `yaml:"technician_group_id"`
}

type PostgresConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
	SSLMode  string `yaml:"sslmode"`
}

type VPNConfig struct {
	// Caminho local para as imagens do guia (capturas de tela do FortiClient)
	ImagesPath string `yaml:"images_path"`

	// Dados exibidos no tutorial enviado ao usuário
	Gateway     string `yaml:"gateway"`      // ex: ssl-vpn.empresa.com.br
	Port        int    `yaml:"port"`         // ex: 8443
	ProfileName string `yaml:"profile_name"` // ex: VPN-Empresa
}

type WebConfig struct {
	Listen   string `yaml:"listen"`
	Username string `yaml:"-"` // WEB_USERNAME (padrão: admin)
	Password string `yaml:"-"` // WEB_PASSWORD
}

var C Config

func Load(path string) {
	// Carrega .env (ignora erro se não existir — usa variáveis do sistema)
	if err := godotenv.Load(); err != nil {
		log.Println("[CONFIG] .env não encontrado, usando variáveis do sistema.")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("[CONFIG] Erro ao ler %s: %v", path, err)
	}

	// Expande ${VAR} com valores do ambiente antes de parsear o YAML
	expanded := os.ExpandEnv(string(data))

	if err := yaml.Unmarshal([]byte(expanded), &C); err != nil {
		log.Fatalf("[CONFIG] Erro ao parsear YAML: %v", err)
	}

	// Aplica padrões para campos obrigatórios não preenchidos
	if C.Bot.CompanyName == "" {
		C.Bot.CompanyName = "Empresa"
	}
	if C.Bot.SupportName == "" {
		C.Bot.SupportName = "Suporte Técnico"
	}
	if C.Bot.Timezone == "" {
		C.Bot.Timezone = "America/Sao_Paulo"
	}
	if C.GLPI.TicketTitle == "" {
		C.GLPI.TicketTitle = "Suporte via WhatsApp"
	}
	if C.VPN.ProfileName == "" {
		C.VPN.ProfileName = "VPN"
	}
	if C.VPN.Port == 0 {
		C.VPN.Port = 443
	}

	// Credenciais do AD
	C.AD.BindPassword = os.Getenv("AD_BIND_PASSWORD")

	// Credenciais SMTP
	C.SMTP.Password = os.Getenv("SMTP_PASSWORD")

	// Credenciais PostgreSQL
	C.Postgres.Password = os.Getenv("POSTGRES_PASSWORD")

	// Teams
	C.Bot.TeamsWebhookURL = os.Getenv("TEAMS_WEBHOOK_URL")

	// GLPI — OAuth2 (GLPI 11+)
	C.GLPI.OAuthClientID = os.Getenv("GLPI_OAUTH_CLIENT_ID")
	C.GLPI.OAuthClientSecret = os.Getenv("GLPI_OAUTH_CLIENT_SECRET")
	C.GLPI.OAuthUsername = os.Getenv("GLPI_OAUTH_USERNAME")
	C.GLPI.OAuthPassword = os.Getenv("GLPI_OAUTH_PASSWORD")

	// GLPI — Legacy (GLPI 10 e anteriores)
	C.GLPI.UserToken = os.Getenv("GLPI_USER_TOKEN")
	C.GLPI.AppToken = os.Getenv("GLPI_APP_TOKEN")

	// Painel web de auditoria
	C.Web.Username = os.Getenv("WEB_USERNAME")
	if C.Web.Username == "" {
		C.Web.Username = "admin"
	}
	C.Web.Password = os.Getenv("WEB_PASSWORD")

	// Tokens dos firewalls: FW_TOKEN_<name> — o <name> é o campo "name" do YAML
	for i := range C.Firewalls {
		envKey := "FW_TOKEN_" + C.Firewalls[i].Name
		C.Firewalls[i].Token = os.Getenv(envKey)
		if C.Firewalls[i].Token == "" {
			log.Printf("[CONFIG] Aviso: token para o firewall %q (variável %s) não encontrado.",
				C.Firewalls[i].Name, envKey)
		}
	}

	log.Println("[CONFIG] Configurações carregadas com sucesso.")
}

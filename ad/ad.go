package ad

import (
	"crypto/tls"
	"fmt"
	"strings"

	"whatsapp-bot/config"

	"github.com/go-ldap/ldap/v3"
)

// UserInfo contém dados básicos de um usuário do AD.
type UserInfo struct {
	DN          string
	SamAccount  string
	DisplayName string
	FirstName   string
	Email       string
	Locked      bool
}

func connect() (*ldap.Conn, error) {
	cfg := config.C.AD

	// Configura TLS — InsecureSkipVerify=true aceita certs self-signed (comum em DCs internos).
	// Mude para false e forneça o CA se quiser validação completa.
	tlsCfg := &tls.Config{
		InsecureSkipVerify: cfg.TLSSkipVerify,
		ServerName:         cfg.TLSServerName,
	}

	var conn *ldap.Conn
	var err error

	if strings.HasPrefix(strings.ToLower(cfg.Host), "ldaps://") {
		// LDAPS — TLS desde o início (porta 636)
		conn, err = ldap.DialURL(cfg.Host, ldap.DialWithTLSConfig(tlsCfg))
	} else {
		// LDAP simples ou STARTTLS (porta 389)
		conn, err = ldap.DialURL(cfg.Host)
		if err == nil && cfg.UseStartTLS {
			if err = conn.StartTLS(tlsCfg); err != nil {
				conn.Close()
				return nil, fmt.Errorf("StartTLS falhou: %w", err)
			}
		}
	}

	if err != nil {
		return nil, fmt.Errorf("falha ao conectar ao AD: %w", err)
	}
	if err := conn.Bind(cfg.BindDN, cfg.BindPassword); err != nil {
		conn.Close()
		return nil, fmt.Errorf("falha no bind LDAP: %w", err)
	}
	return conn, nil
}

// FindUser busca um usuário pela matrícula (sAMAccountName).
// Retorna nil se não encontrado.
func FindUser(matricula string) (*UserInfo, error) {
	conn, err := connect()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	matricula = ldap.EscapeFilter(strings.TrimSpace(matricula))

	req := ldap.NewSearchRequest(
		config.C.AD.BaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		fmt.Sprintf("(&(objectClass=user)(sAMAccountName=%s))", matricula),
		[]string{"dn", "sAMAccountName", "displayName", "givenName", "mail", "userAccountControl"},
		nil,
	)

	res, err := conn.Search(req)
	if err != nil {
		return nil, fmt.Errorf("erro na busca LDAP: %w", err)
	}
	if len(res.Entries) == 0 {
		return nil, nil
	}

	e := res.Entries[0]
	var uac int
	fmt.Sscanf(e.GetAttributeValue("userAccountControl"), "%d", &uac)

	firstName := e.GetAttributeValue("givenName")
	if firstName == "" {
		parts := strings.Fields(e.GetAttributeValue("displayName"))
		if len(parts) > 0 {
			firstName = parts[0]
		}
	}

	return &UserInfo{
		DN:          e.DN,
		SamAccount:  e.GetAttributeValue("sAMAccountName"),
		DisplayName: e.GetAttributeValue("displayName"),
		FirstName:   firstName,
		Email:       e.GetAttributeValue("mail"),
		Locked:      (uac & 0x0010) != 0,
	}, nil
}

// IsMemberOf verifica se o usuário pertence a QUALQUER grupo da lista.
// Usa LDAP_MATCHING_RULE_IN_CHAIN para suportar grupos aninhados.
func IsMemberOf(userDN string, groupsDN []string) (bool, error) {
	conn, err := connect()
	if err != nil {
		return false, err
	}
	defer conn.Close()

	for _, groupDN := range groupsDN {
		filter := fmt.Sprintf(
			"(&(objectClass=group)(distinguishedName=%s)(member:1.2.840.113556.1.4.1941:=%s))",
			ldap.EscapeFilter(groupDN),
			ldap.EscapeFilter(userDN),
		)
		req := ldap.NewSearchRequest(
			config.C.AD.BaseDN,
			ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
			filter, []string{"dn"}, nil,
		)
		sr, err := conn.Search(req)
		if err == nil && len(sr.Entries) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// UnlockUser remove o bloqueio de uma conta (lockoutTime=0).
func UnlockUser(userDN string) error {
	conn, err := connect()
	if err != nil {
		return err
	}
	defer conn.Close()

	mod := ldap.NewModifyRequest(userDN, nil)
	mod.Replace("lockoutTime", []string{"0"})
	return conn.Modify(mod)
}

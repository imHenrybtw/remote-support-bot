package db

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"whatsapp-bot/config"

	_ "github.com/lib/pq"
)

var DB *sql.DB

func Connect() {
	cfg := config.C.Postgres
	dsn := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.DBName, cfg.SSLMode,
	)
	var err error
	DB, err = sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("[DB] Falha ao conectar: %v", err)
	}
	if err = DB.Ping(); err != nil {
		log.Fatalf("[DB] Falha no ping: %v", err)
	}
	DB.SetMaxOpenConns(25)
	DB.SetMaxIdleConns(5)
	DB.SetConnMaxLifetime(5 * time.Minute)
	migrate()
	log.Println("[DB] PostgreSQL conectado.")
}

func migrate() {
	_, err := DB.Exec(`
        -- Solicitações de desbloqueio de matrícula
        CREATE TABLE IF NOT EXISTS unlock_requests (
                id               SERIAL PRIMARY KEY,
                phone            TEXT        NOT NULL,
                requester_id     TEXT        NOT NULL,
                target_id        TEXT        NOT NULL,
                confirm_code     TEXT        NOT NULL,
                result           TEXT        NOT NULL DEFAULT 'pending',
                started_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                finished_at      TIMESTAMPTZ
        );

        -- Solicitações de derrubada de sessão de firewall
        CREATE TABLE IF NOT EXISTS firewall_requests (
                id               SERIAL PRIMARY KEY,
                phone            TEXT        NOT NULL,
                requester_id     TEXT        NOT NULL,
                target_id        TEXT        NOT NULL,
                confirm_code     TEXT        NOT NULL,
                sessions_found   INT         NOT NULL DEFAULT 0,
                sessions_killed  INT         NOT NULL DEFAULT 0,
                result           TEXT        NOT NULL DEFAULT 'pending',
                started_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                finished_at      TIMESTAMPTZ
        );

        -- Chamados abertos via atendente humano
        CREATE TABLE IF NOT EXISTS human_requests (
                id                 SERIAL PRIMARY KEY,
                phone              TEXT        NOT NULL,
                requester_id       TEXT        NOT NULL,
                glpi_ticket_id     INT,
                description        TEXT        NOT NULL,
                attendant_id       TEXT,                   -- número/JID de quem atendeu
                attended_at        TIMESTAMPTZ,            -- quando atendente respondeu pela primeira vez
                last_attendant_msg TIMESTAMPTZ,            -- última mensagem do atendente (controla timer 30min)
                follow_up_sent_at  TIMESTAMPTZ,            -- quando bot enviou pergunta de acompanhamento
                resolved           BOOLEAN     NOT NULL DEFAULT FALSE,
                resolved_at        TIMESTAMPTZ,
                started_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                finished_at        TIMESTAMPTZ
        );

        -- ─────────────────────────────────────────────────────────────────────
        --  Whitelist de telefones autorizados — funcionários em home office.
        --
        --  Apenas números presentes aqui (active = TRUE) podem interagir com
        --  o bot. A tabela é populada/sincronizada a partir da base de RH ou
        --  AD via BulkSyncAllowedPhones(). Soft-delete preserva histórico.
        --
        --  Formato do campo phone: somente dígitos, sem "+55" nem espaços.
        --  Exemplos válidos: "47991234567", "11987654321"
        -- ─────────────────────────────────────────────────────────────────────
        CREATE TABLE IF NOT EXISTS allowed_phones (
                id          SERIAL PRIMARY KEY,
                phone       TEXT        NOT NULL UNIQUE,
                matricula   TEXT,                        -- matrícula do funcionário (para cruzar com AD)
                name        TEXT,                        -- nome completo (auditoria e logs)
                active      BOOLEAN     NOT NULL DEFAULT TRUE,
                added_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
        );

        -- Índice parcial: lookup O(log n) no hot-path de cada mensagem recebida.
        CREATE INDEX IF NOT EXISTS idx_allowed_phones_active ON allowed_phones (phone) WHERE active = TRUE;

        -- Índice para garantir que códigos de confirmação não se repitam
        CREATE UNIQUE INDEX IF NOT EXISTS idx_unlock_confirm_code   ON unlock_requests   (confirm_code);
        CREATE UNIQUE INDEX IF NOT EXISTS idx_firewall_confirm_code ON firewall_requests (confirm_code);

        -- Migrations incrementais (ADD COLUMN IF NOT EXISTS para bancos existentes)
        DO $$ BEGIN
                BEGIN ALTER TABLE human_requests ADD COLUMN attendant_id       TEXT;        EXCEPTION WHEN duplicate_column THEN NULL; END;
                BEGIN ALTER TABLE human_requests ADD COLUMN attended_at        TIMESTAMPTZ; EXCEPTION WHEN duplicate_column THEN NULL; END;
                BEGIN ALTER TABLE human_requests ADD COLUMN last_attendant_msg TIMESTAMPTZ; EXCEPTION WHEN duplicate_column THEN NULL; END;
                BEGIN ALTER TABLE human_requests ADD COLUMN follow_up_sent_at  TIMESTAMPTZ; EXCEPTION WHEN duplicate_column THEN NULL; END;
                BEGIN ALTER TABLE human_requests ADD COLUMN resolved           BOOLEAN NOT NULL DEFAULT FALSE; EXCEPTION WHEN duplicate_column THEN NULL; END;
                BEGIN ALTER TABLE human_requests ADD COLUMN resolved_at        TIMESTAMPTZ; EXCEPTION WHEN duplicate_column THEN NULL; END;
        END $$;
        `)
	if err != nil {
		log.Fatalf("[DB] Migration falhou: %v", err)
	}
}

// CleanupPendingOnStartup marca como expired_restart todos os registros que
// ficaram com result='pending' de uma execução anterior (bot foi reiniciado
// antes de concluir a operação). Deve ser chamado uma única vez no startup.
func CleanupPendingOnStartup() {
	stmts := []string{
		`UPDATE unlock_requests   SET result='expired_restart', finished_at=NOW() WHERE result='pending'`,
		`UPDATE firewall_requests SET result='expired_restart', finished_at=NOW() WHERE result='pending'`,
	}
	total := int64(0)
	for _, q := range stmts {
		res, err := DB.Exec(q)
		if err != nil {
			log.Printf("[DB] CleanupPendingOnStartup falhou: %v", err)
			return
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if total > 0 {
		log.Printf("[DB] %d registro(s) pendente(s) marcado(s) como expired_restart.", total)
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// CodeExists verifica se um código já foi usado em qualquer tabela.
func CodeExists(code string) (bool, error) {
	var n int
	err := DB.QueryRow(
		`SELECT COUNT(*) FROM (
                        SELECT confirm_code FROM unlock_requests   WHERE confirm_code = $1
                        UNION ALL
                        SELECT confirm_code FROM firewall_requests WHERE confirm_code = $1
                ) t`, code,
	).Scan(&n)
	return n > 0, err
}

// ─── Allowed Phones (whitelist home office) ───────────────────────────────────

// AllowedPhone representa um funcionário autorizado a usar o bot.
type AllowedPhone struct {
	ID        int
	Phone     string
	Matricula string
	Name      string
	Active    bool
	AddedAt   time.Time
	UpdatedAt time.Time
}

// IsPhoneAllowed verifica se o número está na whitelist e está ativo.
// Chamado no hot-path de cada mensagem recebida — índice parcial garante
// lookup O(log n) mesmo com milhares de funcionários cadastrados.
func IsPhoneAllowed(phone string) (bool, error) {
	var exists bool
	err := DB.QueryRow(
		`SELECT EXISTS (
                        SELECT 1 FROM allowed_phones
                        WHERE phone = $1 AND active = TRUE
                )`, phone,
	).Scan(&exists)
	return exists, err
}

// AddAllowedPhone insere ou reativa um número na whitelist.
// Se o número já existir (inativo), reativa e atualiza nome/matrícula.
func AddAllowedPhone(phone, matricula, name string) error {
	_, err := DB.Exec(`
                INSERT INTO allowed_phones (phone, matricula, name, active, updated_at)
                VALUES ($1, $2, $3, TRUE, NOW())
                ON CONFLICT (phone) DO UPDATE
                        SET matricula  = EXCLUDED.matricula,
                            name       = EXCLUDED.name,
                            active     = TRUE,
                            updated_at = NOW()
        `, phone, matricula, name)
	return err
}

// DeactivatePhone desativa (soft-delete) um número da whitelist sem apagar
// o histórico. Use quando o funcionário sair do regime home office ou da empresa.
func DeactivatePhone(phone string) error {
	_, err := DB.Exec(
		`UPDATE allowed_phones SET active = FALSE, updated_at = NOW() WHERE phone = $1`,
		phone,
	)
	return err
}

// ListAllowedPhones retorna todos os registros (ativos e inativos).
// Útil para o painel de auditoria e para conferência com o RH.
func ListAllowedPhones() ([]AllowedPhone, error) {
	rows, err := DB.Query(`
                SELECT id, phone, COALESCE(matricula,''), COALESCE(name,''), active, added_at, updated_at
                FROM allowed_phones
                ORDER BY name ASC, phone ASC
        `)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []AllowedPhone
	for rows.Next() {
		var ap AllowedPhone
		if err := rows.Scan(
			&ap.ID, &ap.Phone, &ap.Matricula, &ap.Name,
			&ap.Active, &ap.AddedAt, &ap.UpdatedAt,
		); err != nil {
			return nil, err
		}
		list = append(list, ap)
	}
	return list, rows.Err()
}

// BulkSyncAllowedPhones substitui a whitelist de forma atômica dentro de uma
// transação. Telefones ausentes da nova lista são desativados (soft-delete);
// telefones novos são inseridos; existentes têm nome/matrícula atualizados.
//
// Uso: chamado pelo job de sincronização agendado com a base de RH/AD.
//
//	entries, _ := rh.FetchHomeOfficeEmployees()  // lista vinda do RH
//	db.BulkSyncAllowedPhones(entries)
func BulkSyncAllowedPhones(entries []AllowedPhone) error {
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// 1. Desativa todos os registros existentes
	if _, err := tx.Exec(`UPDATE allowed_phones SET active = FALSE, updated_at = NOW()`); err != nil {
		return err
	}

	// 2. Upsert dos funcionários que chegaram na nova lista
	stmt, err := tx.Prepare(`
                INSERT INTO allowed_phones (phone, matricula, name, active, updated_at)
                VALUES ($1, $2, $3, TRUE, NOW())
                ON CONFLICT (phone) DO UPDATE
                        SET matricula  = EXCLUDED.matricula,
                            name       = EXCLUDED.name,
                            active     = TRUE,
                            updated_at = NOW()
        `)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range entries {
		if _, err := stmt.Exec(e.Phone, e.Matricula, e.Name); err != nil {
			return fmt.Errorf("upsert %s: %w", e.Phone, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("[DB] BulkSync: whitelist atualizada com %d funcionário(s).", len(entries))
	return nil
}

// ─── Unlock ───────────────────────────────────────────────────────────────────

type UnlockRequest struct {
	ID          int
	Phone       string
	RequesterID string
	TargetID    string
	ConfirmCode string
	Result      string
	StartedAt   time.Time
	FinishedAt  *time.Time
}

func InsertUnlockRequest(r UnlockRequest) (int, error) {
	var id int
	err := DB.QueryRow(`
                INSERT INTO unlock_requests (phone, requester_id, target_id, confirm_code, result)
                VALUES ($1,$2,$3,$4,'pending') RETURNING id`,
		r.Phone, r.RequesterID, r.TargetID, r.ConfirmCode,
	).Scan(&id)
	return id, err
}

func FinishUnlockRequest(id int, result string) error {
	_, err := DB.Exec(
		`UPDATE unlock_requests SET result=$1, finished_at=NOW() WHERE id=$2`,
		result, id)
	return err
}

// ─── Firewall ─────────────────────────────────────────────────────────────────

type FirewallRequest struct {
	ID             int
	Phone          string
	RequesterID    string
	TargetID       string
	ConfirmCode    string
	SessionsFound  int
	SessionsKilled int
	Result         string
	StartedAt      time.Time
	FinishedAt     *time.Time
}

func InsertFirewallRequest(r FirewallRequest) (int, error) {
	var id int
	err := DB.QueryRow(`
                INSERT INTO firewall_requests (phone, requester_id, target_id, confirm_code, sessions_found, result)
                VALUES ($1,$2,$3,$4,$5,'pending') RETURNING id`,
		r.Phone, r.RequesterID, r.TargetID, r.ConfirmCode, r.SessionsFound,
	).Scan(&id)
	return id, err
}

func FinishFirewallRequest(id int, killed int, result string) error {
	_, err := DB.Exec(
		`UPDATE firewall_requests SET sessions_killed=$1, result=$2, finished_at=NOW() WHERE id=$3`,
		killed, result, id)
	return err
}

// ─── Human ────────────────────────────────────────────────────────────────────

type HumanRequest struct {
	ID           int
	Phone        string
	RequesterID  string
	GLPITicketID *int
	Description  string
	AttendantID  string
	StartedAt    time.Time
	FinishedAt   *time.Time
}

func InsertHumanRequest(r HumanRequest) (int, error) {
	var id int
	err := DB.QueryRow(`
                INSERT INTO human_requests (phone, requester_id, description)
                VALUES ($1,$2,$3) RETURNING id`,
		r.Phone, r.RequesterID, r.Description,
	).Scan(&id)
	return id, err
}

func SetHumanGLPI(id int, glpiID int) error {
	_, err := DB.Exec(
		`UPDATE human_requests SET glpi_ticket_id=$1 WHERE id=$2`,
		glpiID, id)
	return err
}

// MarkAttended registra que um atendente iniciou o atendimento.
func MarkAttended(id int, attendantID string) error {
	_, err := DB.Exec(`
                UPDATE human_requests
                SET attendant_id=$1, attended_at=NOW(), last_attendant_msg=NOW()
                WHERE id=$2`,
		attendantID, id)
	return err
}

// UpdateLastAttendantMsg atualiza o timestamp da última mensagem do atendente.
func UpdateLastAttendantMsg(id int) error {
	_, err := DB.Exec(
		`UPDATE human_requests SET last_attendant_msg=NOW() WHERE id=$1`,
		id)
	return err
}

// MarkFollowUpSent registra que o bot enviou a pergunta de acompanhamento.
func MarkFollowUpSent(id int) error {
	_, err := DB.Exec(
		`UPDATE human_requests SET follow_up_sent_at=NOW() WHERE id=$1`,
		id)
	return err
}

// MarkResolved registra resolução e fecha o atendimento.
func MarkResolved(id int) error {
	_, err := DB.Exec(`
                UPDATE human_requests SET resolved=TRUE, resolved_at=NOW(), finished_at=NOW() WHERE id=$1`,
		id)
	return err
}

// MarkUnresolved reabre o atendimento (usuário disse que não foi resolvido).
func MarkUnresolved(id int) error {
	_, err := DB.Exec(
		`UPDATE human_requests SET resolved=FALSE, follow_up_sent_at=NULL WHERE id=$1`,
		id)
	return err
}

func FinishHumanRequest(id int, glpiID *int) error {
	_, err := DB.Exec(
		`UPDATE human_requests SET glpi_ticket_id=$1, finished_at=NOW() WHERE id=$2`,
		glpiID, id)
	return err
}

// CloseHumanRequest marca o atendimento como encerrado sem alterar o glpi_ticket_id já registrado.
func CloseHumanRequest(id int) error {
	_, err := DB.Exec(
		`UPDATE human_requests SET finished_at=NOW() WHERE id=$1 AND finished_at IS NULL`,
		id)
	return err
}

package bot

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"whatsapp-bot/ad"
	"whatsapp-bot/config"
	"whatsapp-bot/db"
	"whatsapp-bot/fortigate"
	"whatsapp-bot/glpi"
	"whatsapp-bot/mail"
	"whatsapp-bot/teams"
	"whatsapp-bot/vpn"
)

// ═══════════════════════════════════════════════════════════════
//  RATE LIMITER — proteção contra abuso/DDoS via WhatsApp
// ═══════════════════════════════════════════════════════════════

const (
	rlWindow  = 60 * time.Second
	rlMaxMsgs = 10 // máx. mensagens por JID por minuto
)

type rateLimiter struct {
	mu     sync.Mutex
	window map[string][]time.Time
}

var rl = &rateLimiter{window: make(map[string][]time.Time)}

func (r *rateLimiter) allow(jid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-rlWindow)

	prev := r.window[jid]
	filtered := prev[:0]
	for _, t := range prev {
		if t.After(cutoff) {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) >= rlMaxMsgs {
		r.window[jid] = filtered
		return false
	}
	r.window[jid] = append(filtered, now)
	return true
}

// PurgeRateLimiter remove entradas antigas do rate limiter (chamar a cada minuto).
func PurgeRateLimiter() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := time.Now().Add(-rlWindow)
	for jid, times := range rl.window {
		active := false
		for _, t := range times {
			if t.After(cutoff) {
				active = true
				break
			}
		}
		if !active {
			delete(rl.window, jid)
		}
	}
}

// ═══════════════════════════════════════════════════════════════
//  ESTADOS
// ═══════════════════════════════════════════════════════════════

type State int

const (
	StateGreeting State = iota
	StateMainMenu
	StateVPNStep
	StateVPNConfigOK
	StateVPNNeedHelp
	StateVPNDescribe
	StateUnlockTarget
	StateUnlockCode
	StateFirewallTarget // pede matrícula alvo
	StateFirewallCode   // aguarda código de confirmação
	StateHumanDescribe
	StateHumanWaiting
	StateAttended // atendente respondeu — bot silenciado
	StateFollowUp // bot enviou pergunta de acompanhamento
	StateDone
)

// ═══════════════════════════════════════════════════════════════
//  SESSÃO
// ═══════════════════════════════════════════════════════════════

type Session struct {
	State   State
	JID     string
	Phone   string
	IsAdmin bool // membro de G_ADUnlock → vê menu completo

	// Dados do solicitante
	Matricula string
	FirstName string
	UserInfo  *ad.UserInfo

	// VPN
	VPNStep int

	// Unlock
	TargetMatricula string
	TargetInfo      *ad.UserInfo
	DBUnlockID      int

	// Firewall
	FWTargetMatricula string
	FWSessions        []fortigate.UserSession
	DBFirewallID      int

	// Confirmação (unlock e firewall)
	ConfirmCode     string
	ConfirmExpires  time.Time
	ConfirmAttempts int // bloqueia após 3 tentativas erradas

	// Atendente humano
	DBHumanID        int
	GLPITicketID     int
	HumanDeadline    time.Time
	AttendantID      string    // JID/número do atendente que respondeu
	AttendantLastMsg time.Time // última mensagem do atendente (timer 30min)

	// Controle de inatividade
	LastActivity time.Time
}

var (
	sessions   = make(map[string]*Session)
	sessionsMu sync.Mutex
)

func getSessionWithFlag(jid, phone string) (*Session, bool) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	if s, ok := sessions[jid]; ok {
		s.LastActivity = time.Now()
		return s, false
	}
	s := &Session{
		State:        StateGreeting,
		JID:          jid,
		Phone:        phone,
		LastActivity: time.Now(),
	}
	sessions[jid] = s
	return s, true
}

func resetSession(jid, phone string) *Session {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	s := &Session{
		State:        StateGreeting,
		JID:          jid,
		Phone:        phone,
		LastActivity: time.Now(),
	}
	sessions[jid] = s
	return s
}

// ═══════════════════════════════════════════════════════════════
//  TIPOS DE MENSAGEM
// ═══════════════════════════════════════════════════════════════

type MsgType int

const (
	MsgText  MsgType = iota
	MsgImage         // envia imagem do disco local
)

type Message struct {
	Type      MsgType
	Text      string
	ImageFile string // nome do arquivo em vpn.ImagePath()
}

type Response struct {
	Messages []Message
}

func txt(t string) Response {
	return Response{Messages: []Message{{Type: MsgText, Text: t}}}
}

func imgStep(imageFile, caption string) Message {
	return Message{Type: MsgImage, ImageFile: imageFile, Text: caption}
}

// ═══════════════════════════════════════════════════════════════
//  HORÁRIO DE ATENDIMENTO
// ═══════════════════════════════════════════════════════════════

func isWorkingHours() bool {
	tz := config.C.Bot.Timezone
	if tz == "" {
		tz = "America/Sao_Paulo"
	}
	loc, _ := time.LoadLocation(tz)
	now := time.Now().In(loc)

	cfg := config.C.Bot
	dayOK := false
	for _, d := range cfg.WorkingDays {
		if int(now.Weekday()) == d {
			dayOK = true
			break
		}
	}
	if !dayOK {
		return false
	}

	var sh, sm, eh, em int
	fmt.Sscanf(cfg.WorkingHoursStart, "%d:%d", &sh, &sm)

	// Sábado tem horário diferente
	endStr := cfg.WorkingHoursEnd
	if int(now.Weekday()) == 6 && cfg.SaturdayHoursEnd != "" {
		endStr = cfg.SaturdayHoursEnd
	}
	fmt.Sscanf(endStr, "%d:%d", &eh, &em)

	start := time.Date(now.Year(), now.Month(), now.Day(), sh, sm, 0, 0, loc)
	end := time.Date(now.Year(), now.Month(), now.Day(), eh, em, 0, 0, loc)
	return now.After(start) && now.Before(end)
}

func workingHoursText() string {
	cfg := config.C.Bot
	days := map[int]string{0: "Dom", 1: "Seg", 2: "Ter", 3: "Qua", 4: "Qui", 5: "Sex", 6: "Sáb"}
	var dayNames []string
	for _, d := range cfg.WorkingDays {
		dayNames = append(dayNames, days[d])
	}
	satLine := ""
	if cfg.SaturdayHoursEnd != "" {
		satLine = fmt.Sprintf("\nSábado: %s às %s", cfg.WorkingHoursStart, cfg.SaturdayHoursEnd)
	}
	if len(dayNames) < 2 {
		return fmt.Sprintf("%s: %s às %s%s", dayNames[0], cfg.WorkingHoursStart, cfg.WorkingHoursEnd, satLine)
	}
	return fmt.Sprintf("%s a %s: %s às %s%s",
		dayNames[0], dayNames[len(dayNames)-2],
		cfg.WorkingHoursStart, cfg.WorkingHoursEnd, satLine)
}

func urgencyPhones() string {
	return strings.Join(config.C.Bot.UrgencyPhone, " | ")
}

// ═══════════════════════════════════════════════════════════════
//  GERAÇÃO DE CÓDIGO ÚNICO
// ═══════════════════════════════════════════════════════════════

const codeChars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func genUniqueCode() (string, error) {
	for attempts := 0; attempts < 10; attempts++ {
		b := make([]byte, 6)
		for i := range b {
			n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(codeChars))))
			b[i] = codeChars[n.Int64()]
		}
		code := string(b)
		exists, err := db.CodeExists(code)
		if err != nil || !exists {
			return code, err
		}
	}
	return "", fmt.Errorf("não foi possível gerar código único")
}

// ═══════════════════════════════════════════════════════════════
//  MENUS
// ═══════════════════════════════════════════════════════════════

func mainMenuAdmin(firstName string) Response {
	return txt(fmt.Sprintf(
		"✅ Olá, *%s*! Como posso te ajudar?\n\n"+
			"1️⃣  🔒 Problemas com VPN\n"+
			"2️⃣  🔓 Desbloqueio de Matrícula\n"+
			"3️⃣  🌐 Derrubada de Sessão de Firewall\n"+
			"4️⃣  👨‍💼 Falar com Atendente\n"+
			"5️⃣  🚪 Encerrar Atendimento",
		firstName,
	))
}

func mainMenuBasic(firstName string) Response {
	return txt(fmt.Sprintf(
		"✅ Olá, *%s*! Como posso te ajudar?\n\n"+
			"1️⃣  🔒 Problemas com VPN\n"+
			"2️⃣  👨‍💼 Falar com Atendente\n"+
			"3️⃣  🚪 Encerrar Atendimento",
		firstName,
	))
}

// ═══════════════════════════════════════════════════════════════
//  ENTRADA PRINCIPAL
// ═══════════════════════════════════════════════════════════════

func Handle(jid, phone, input string) Response {
	// Rate limiting: drop silenciosamente se exceder rlMaxMsgs por minuto
	if !rl.allow(jid) {
		fmt.Printf("[BOT] Rate limit atingido para %s\n", phone)
		return Response{}
	}

	input = strings.TrimSpace(input)
	lower := strings.ToLower(input)

	s, isNew := getSessionWithFlag(jid, phone)

	// Bloqueia reset durante atendimento humano ativo
	if lower == "menu" || lower == "reiniciar" {
		if !isNew && (s.State == StateHumanWaiting || s.State == StateAttended || s.State == StateFollowUp) {
			return txt("⚠️ Você possui um atendimento em andamento. Aguarde o retorno do técnico.\n\nUrgências: 📞 " + urgencyPhones())
		}
		resetSession(jid, phone)
		return greetNew()
	}

	if isNew {
		return greetNew()
	}

	switch s.State {
	case StateGreeting:
		return handleGreeting(s, input)
	case StateMainMenu:
		return handleMainMenu(s, input)
	case StateVPNStep:
		return handleVPNStep(s, input)
	case StateVPNConfigOK:
		return handleVPNConfigOK(s, input)
	case StateVPNNeedHelp:
		return handleVPNNeedHelp(s, input)
	case StateVPNDescribe:
		return handleVPNDescribe(s, input)
	case StateUnlockTarget:
		return handleUnlockTarget(s, input)
	case StateUnlockCode:
		return handleUnlockCode(s, input)
	case StateFirewallTarget:
		return handleFirewallTarget(s, input)
	case StateFirewallCode:
		return handleFirewallCode(s, input)
	case StateHumanDescribe:
		return handleHumanDescribe(s, input)
	case StateHumanWaiting:
		// Silenciado — chamado aberto, aguardando atendente
		return Response{}
	case StateAttended:
		// Bot silenciado — atendente está respondendo. Não interagir.
		return Response{}
	case StateFollowUp:
		return handleFollowUp(s, input)
	case StateDone:
		// Qualquer nova mensagem reinicia
		ns := resetSession(jid, phone)
		_ = ns
		return greetNew()
	}

	return greetNew()
}

// ═══════════════════════════════════════════════════════════════
//  SAUDAÇÃO
// ═══════════════════════════════════════════════════════════════

func greetNew() Response {
	supportName := config.C.Bot.SupportName
	if supportName == "" {
		supportName = "Suporte Técnico"
	}
	if isWorkingHours() {
		return txt(fmt.Sprintf(
			"👋 *Bem-vindo ao %s!*\n\n"+
				"🕐 *Horário de atendimento:*\n%s\n\n"+
				"Para continuar, informe sua *matrícula*:",
			supportName, workingHoursText(),
		))
	}
	return txt(fmt.Sprintf(
		"👋 *Olá! Obrigado por entrar em contato.*\n\n"+
			"😔 Estamos *fora do horário de atendimento*.\n\n"+
			"🕐 *Horário:*\n%s\n\n"+
			"📋 Registre sua solicitação:\n🔗 %s\n\n"+
			"🚨 Urgências: 📞 %s",
		workingHoursText(), config.C.GLPI.URL, urgencyPhones(),
	))
}

func handleGreeting(s *Session, input string) Response {
	if !isWorkingHours() {
		return greetNew()
	}

	user, err := ad.FindUser(input)
	if err != nil {
		fmt.Printf("[BOT] AD FindUser %q: %v\n", input, err)
		return txt("⚠️ Não foi possível verificar sua matrícula. Tente novamente:")
	}
	if user == nil {
		return txt(fmt.Sprintf("❌ Matrícula *%s* não encontrada.\n\nVerifique e tente novamente:", input))
	}

	s.Matricula = input
	s.UserInfo = user
	s.FirstName = user.FirstName

	// Verifica se é admin (grupo G_ADUnlock)
	isAdmin, err := ad.IsMemberOf(user.DN, config.C.AD.GroupUnlock)
	if err != nil {
		fmt.Printf("[BOT] Erro ao verificar grupo: %v\n", err)
	}
	s.IsAdmin = isAdmin
	s.State = StateMainMenu

	if isAdmin {
		return mainMenuAdmin(user.FirstName)
	}
	return mainMenuBasic(user.FirstName)
}

// ═══════════════════════════════════════════════════════════════
//  MENU PRINCIPAL
// ═══════════════════════════════════════════════════════════════

func handleMainMenu(s *Session, input string) Response {
	n := strings.TrimSpace(input)

	if s.IsAdmin {
		switch n {
		case "1":
			s.State = StateVPNStep
			s.VPNStep = 0
			return sendVPNStep(s)
		case "2":
			s.State = StateUnlockTarget
			return txt("🔓 *Desbloqueio de Matrícula*\n\nInforme a *matrícula* que deseja desbloquear:")
		case "3":
			s.State = StateFirewallTarget
			return txt("🌐 *Derrubada de Sessão de Firewall*\n\nInforme a *matrícula* do usuário a ser desconectado:")
		case "4":
			s.State = StateHumanDescribe
			return txt("👨‍💼 *Falar com Atendente*\n\nDescreva o problema:")
		case "5":
			s.State = StateDone
			return txt(fmt.Sprintf("👋 Atendimento encerrado. Obrigado, *%s*!\n\nQualquer nova mensagem reinicia o atendimento.", s.FirstName))
		}
	} else {
		switch n {
		case "1":
			s.State = StateVPNStep
			s.VPNStep = 0
			return sendVPNStep(s)
		case "2":
			s.State = StateHumanDescribe
			return txt("👨‍💼 *Falar com Atendente*\n\nDescreva o problema:")
		case "3":
			s.State = StateDone
			return txt(fmt.Sprintf("👋 Atendimento encerrado. Obrigado, *%s*!\n\nQualquer nova mensagem reinicia o atendimento.", s.FirstName))
		}
	}

	if s.IsAdmin {
		return mainMenuAdmin(s.FirstName)
	}
	return mainMenuBasic(s.FirstName)
}

// ═══════════════════════════════════════════════════════════════
//  FLUXO VPN
// ═══════════════════════════════════════════════════════════════

func sendVPNStep(s *Session) Response {
	if s.VPNStep >= len(vpn.Guide) {
		s.State = StateVPNConfigOK
		return txt("✅ *Esses foram todos os passos!*\n\nA configuração da VPN está correta agora?\n\n1️⃣  Sim\n2️⃣  Não")
	}

	step := vpn.Guide[s.VPNStep]
	s.VPNStep++
	nav := fmt.Sprintf("\n\n_Passo %d de %d_ — Digite *próximo* para avançar.", s.VPNStep, len(vpn.Guide))

	var msgs []Message
	if vpn.ImageExists(step.ImageFile) {
		msgs = append(msgs, imgStep(step.ImageFile, step.Text+nav))
	} else {
		msgs = append(msgs, Message{Type: MsgText, Text: step.Text + nav})
	}
	return Response{Messages: msgs}
}

func handleVPNStep(s *Session, input string) Response {
	n := strings.ToLower(strings.TrimSpace(input))
	if n == "próximo" || n == "proximo" || n == "p" || n == "ok" {
		return sendVPNStep(s)
	}
	return txt("Digite *próximo* para avançar ao próximo passo.")
}

func handleVPNConfigOK(s *Session, input string) Response {
	switch strings.TrimSpace(input) {
	case "1":
		s.State = StateVPNNeedHelp
		return txt("👍 A VPN está funcionando corretamente agora?\n\n1️⃣  Sim, está funcionando!\n2️⃣  Ainda preciso de ajuda")
	case "2":
		s.State = StateVPNNeedHelp
		return txt("😕 Entendido. Deseja falar com um atendente?\n\n1️⃣  Não, vou tentar novamente\n2️⃣  Sim, quero atendente")
	}
	return txt("Por favor, responda *1* (Sim) ou *2* (Não).")
}

func handleVPNNeedHelp(s *Session, input string) Response {
	switch strings.TrimSpace(input) {
	case "1":
		s.State = StateDone
		return txt(fmt.Sprintf("✅ *Ótimo!* Fico feliz que funcionou, *%s*!\n\nQualquer nova mensagem reinicia o atendimento.", s.FirstName))
	case "2":
		s.State = StateVPNDescribe
		return txt("📝 Descreva o problema com a VPN para que o técnico já tenha o contexto:")
	}
	return txt("Por favor, responda *1* ou *2*.")
}

func handleVPNDescribe(s *Session, input string) Response {
	return openHumanTicket(s, input)
}

// ═══════════════════════════════════════════════════════════════
//  FLUXO DESBLOQUEIO
// ═══════════════════════════════════════════════════════════════

func handleUnlockTarget(s *Session, input string) Response {
	target, err := ad.FindUser(input)
	if err != nil {
		fmt.Printf("[BOT] AD FindUser %q: %v\n", input, err)
		return txt("⚠️ Não foi possível verificar a matrícula. Tente novamente:")
	}
	if target == nil {
		return txt(fmt.Sprintf("❌ Matrícula *%s* não encontrada.\n\nVerifique e tente novamente:", input))
	}

	if !target.Locked {
		return txt(fmt.Sprintf(
			"ℹ️ A conta *%s* (%s) já está *desbloqueada* no Active Directory.\n\nNenhuma ação necessária.",
			target.DisplayName, input,
		))
	}

	s.TargetMatricula = input
	s.TargetInfo = target

	if s.UserInfo == nil || s.UserInfo.Email == "" {
		return txt("⚠️ Seu usuário não possui e-mail cadastrado. Não é possível enviar o código.")
	}

	code, err := genUniqueCode()
	if err != nil {
		return txt("⚠️ Erro ao gerar código de confirmação. Tente novamente.")
	}
	s.ConfirmCode = code
	s.ConfirmExpires = time.Now().Add(10 * time.Minute)

	dbID, _ := db.InsertUnlockRequest(db.UnlockRequest{
		Phone:       s.Phone,
		RequesterID: s.Matricula,
		TargetID:    input,
		ConfirmCode: code,
	})
	s.DBUnlockID = dbID

	if err := mail.SendUnlockCode(s.UserInfo.Email, s.FirstName, target.DisplayName, input, code); err != nil {
		fmt.Printf("[BOT] Falha ao enviar e-mail de desbloqueio para %s: %v\n", s.UserInfo.Email, err)
		db.FinishUnlockRequest(s.DBUnlockID, "error_email")
		return txt("⚠️ Falha ao enviar o código por e-mail. Contate o suporte de TI.")
	}

	s.State = StateUnlockCode
	return txt(fmt.Sprintf(
		"📧 *Código enviado para o seu e-mail!*\n\nConta a desbloquear: *%s* (%s)\n\nInforme o código de 6 caracteres:\n_(Expira em 10 minutos)_",
		target.DisplayName, input,
	))
}

func handleUnlockCode(s *Session, input string) Response {
	if time.Now().After(s.ConfirmExpires) {
		db.FinishUnlockRequest(s.DBUnlockID, "expired")
		s.State = StateDone
		return txt("⌛ *Código expirado.* Operação cancelada.\n\nEnvie uma nova mensagem para reiniciar o atendimento.")
	}
	if strings.ToUpper(strings.TrimSpace(input)) != s.ConfirmCode {
		s.ConfirmAttempts++
		if s.ConfirmAttempts >= 3 {
			fmt.Printf("[AUDIT] UNLOCK BLOQUEADO | solicitante=%s phone=%s alvo=%s tentativas=3\n",
				s.Matricula, s.Phone, s.TargetMatricula)
			db.FinishUnlockRequest(s.DBUnlockID, "locked_attempts")
			s.State = StateDone
			return txt("🔒 *Operação cancelada* após 3 tentativas incorretas.\n\nEnvie uma nova mensagem para reiniciar o atendimento.")
		}
		remaining := 3 - s.ConfirmAttempts
		return txt(fmt.Sprintf("❌ Código incorreto. Você tem mais *%d* tentativa(s):", remaining))
	}

	if err := ad.UnlockUser(s.TargetInfo.DN); err != nil {
		fmt.Printf("[AUDIT] UNLOCK FALHOU | solicitante=%s phone=%s alvo=%s erro=%v\n",
			s.Matricula, s.Phone, s.TargetMatricula, err)
		db.FinishUnlockRequest(s.DBUnlockID, "error_ad")
		s.State = StateDone
		return txt("⚠️ Não foi possível desbloquear a conta. Contate o suporte de TI.")
	}

	db.FinishUnlockRequest(s.DBUnlockID, "confirmed")
	name, mat := s.TargetInfo.DisplayName, s.TargetMatricula
	s.State = StateDone

	return txt(fmt.Sprintf(
		"✅ *Conta desbloqueada com sucesso!*\n\n👤 %s (%s)\n\nO usuário já pode realizar o login.\n\nQualquer nova mensagem reinicia o atendimento.",
		name, mat,
	))
}

// ═══════════════════════════════════════════════════════════════
//  FLUXO FIREWALL (derrubada de sessão)
// ═══════════════════════════════════════════════════════════════

func handleFirewallTarget(s *Session, input string) Response {
	// Valida que a matrícula existe no AD
	target, err := ad.FindUser(input)
	if err != nil {
		fmt.Printf("[BOT] AD FindUser %q: %v\n", input, err)
		return txt("⚠️ Não foi possível verificar a matrícula. Tente novamente:")
	}
	if target == nil {
		return txt(fmt.Sprintf("❌ Matrícula *%s* não encontrada no AD.\n\nVerifique e tente novamente:", input))
	}

	s.FWTargetMatricula = input

	// Busca sessões ativas em todos os firewalls
	sessions, err := fortigate.GetUserSessions(input)
	if err != nil {
		fmt.Printf("[BOT] Erro ao consultar firewalls para %q: %v\n", input, err)
		return txt("⚠️ Não foi possível consultar as sessões de firewall. Tente novamente.")
	}
	if len(sessions) == 0 {
		s.State = StateMainMenu
		if s.IsAdmin {
			return txt(fmt.Sprintf("ℹ️ Nenhuma sessão ativa encontrada para *%s* em nenhum firewall.\n\n", input) +
				fmt.Sprintf("✅ Olá, *%s*! Como posso te ajudar?\n\n1️⃣  🔒 Problemas com VPN\n2️⃣  🔓 Desbloqueio de Matrícula\n3️⃣  🌐 Derrubada de Sessão de Firewall\n4️⃣  👨‍💼 Falar com Atendente\n5️⃣  🚪 Encerrar Atendimento", s.FirstName))
		}
		return mainMenuBasic(s.FirstName)
	}

	s.FWSessions = sessions

	if s.UserInfo == nil || s.UserInfo.Email == "" {
		return txt("⚠️ Seu usuário não possui e-mail cadastrado. Não é possível enviar o código.")
	}

	code, err := genUniqueCode()
	if err != nil {
		return txt("⚠️ Erro ao gerar código de confirmação. Tente novamente.")
	}
	s.ConfirmCode = code
	s.ConfirmExpires = time.Now().Add(10 * time.Minute)

	dbID, _ := db.InsertFirewallRequest(db.FirewallRequest{
		Phone:         s.Phone,
		RequesterID:   s.Matricula,
		TargetID:      input,
		ConfirmCode:   code,
		SessionsFound: len(sessions),
	})
	s.DBFirewallID = dbID

	if err := mail.SendFirewallCode(s.UserInfo.Email, s.FirstName, input, len(sessions), code); err != nil {
		fmt.Printf("[BOT] Falha ao enviar e-mail de firewall para %s: %v\n", s.UserInfo.Email, err)
		db.FinishFirewallRequest(s.DBFirewallID, 0, "error_email")
		return txt("⚠️ Falha ao enviar o código por e-mail. Contate o suporte de TI.")
	}

	s.State = StateFirewallCode
	return txt(fmt.Sprintf(
		"🌐 *Sessões encontradas para %s:* *%d sessão(ões) ativa(s)*\n\n"+
			"📧 Código de confirmação enviado para o seu e-mail.\n\n"+
			"Informe o código de 6 caracteres para confirmar a derrubada:\n"+
			"_(Expira em 10 minutos)_",
		input, len(sessions),
	))
}

func handleFirewallCode(s *Session, input string) Response {
	if time.Now().After(s.ConfirmExpires) {
		db.FinishFirewallRequest(s.DBFirewallID, 0, "expired")
		s.State = StateDone
		return txt("⌛ *Código expirado.* Operação cancelada.\n\nEnvie uma nova mensagem para reiniciar o atendimento.")
	}
	if strings.ToUpper(strings.TrimSpace(input)) != s.ConfirmCode {
		s.ConfirmAttempts++
		if s.ConfirmAttempts >= 3 {
			fmt.Printf("[AUDIT] FIREWALL BLOQUEADO | solicitante=%s phone=%s alvo=%s tentativas=3\n",
				s.Matricula, s.Phone, s.FWTargetMatricula)
			db.FinishFirewallRequest(s.DBFirewallID, 0, "locked_attempts")
			s.State = StateDone
			return txt("🔒 *Operação cancelada* após 3 tentativas incorretas.\n\nEnvie uma nova mensagem para reiniciar o atendimento.")
		}
		remaining := 3 - s.ConfirmAttempts
		return txt(fmt.Sprintf("❌ Código incorreto. Você tem mais *%d* tentativa(s):", remaining))
	}

	result := fortigate.DeauthSessions(s.FWSessions)
	db.FinishFirewallRequest(s.DBFirewallID, result.Success, "confirmado")
	mat := s.FWTargetMatricula
	s.State = StateDone

	msg := fmt.Sprintf(
		"✅ *Derrubada de sessão concluída!*\n\n"+
			"👤 Matrícula: *%s*\n"+
			"📊 Sessões derrubadas: *%d de %d*",
		mat, result.Success, result.Total,
	)
	if result.Errors > 0 {
		msg += fmt.Sprintf("\n⚠️ Erros: %d (verifique os logs)", result.Errors)
	}
	msg += "\n\nQualquer nova mensagem reinicia o atendimento."

	return txt(msg)
}

// ═══════════════════════════════════════════════════════════════
//  FLUXO ATENDENTE HUMANO
// ═══════════════════════════════════════════════════════════════

func handleHumanDescribe(s *Session, input string) Response {
	return openHumanTicket(s, input)
}

func openHumanTicket(s *Session, description string) Response {
	ticketID, err := glpi.OpenTicket(s.Phone, s.Matricula, description)
	if err != nil {
		fmt.Printf("[GLPI] Erro: %v\n", err)
	}

	dbID, _ := db.InsertHumanRequest(db.HumanRequest{
		Phone:       s.Phone,
		RequesterID: s.Matricula,
		Description: description,
	})
	if ticketID > 0 {
		db.SetHumanGLPI(dbID, ticketID)
	}

	s.DBHumanID = dbID
	s.GLPITicketID = ticketID
	s.HumanDeadline = time.Now().Add(time.Duration(config.C.Bot.HumanTimeoutMinutes) * time.Minute)
	s.State = StateHumanWaiting

	// Notifica Teams
	go teams.SendAlert(s.UserInfo.DisplayName, s.Matricula, s.Phone, description, ticketID)

	var msg string
	if ticketID > 0 {
		msg = fmt.Sprintf(
			"✅ *Chamado registrado com sucesso!*\n\n"+
				"🎫 *Chamado GLPI:* #%d\n\n"+
				"⏳ Um técnico entrará em contato em breve.\n\n"+
				"Urgências: 📞 %s",
			ticketID, urgencyPhones(),
		)
	} else {
		msg = fmt.Sprintf(
			"✅ *Chamado registrado com sucesso!*\n\n"+
				"⏳ Um técnico entrará em contato em breve.\n\n"+
				"Urgências: 📞 %s",
			urgencyPhones(),
		)
	}
	return txt(msg)
}

// ═══════════════════════════════════════════════════════════════
//  FLUXO DE ACOMPANHAMENTO PÓS-ATENDIMENTO
// ═══════════════════════════════════════════════════════════════

// handleFollowUp processa a resposta do usuário à pergunta de acompanhamento.
func handleFollowUp(s *Session, input string) Response {
	switch strings.TrimSpace(input) {
	case "1": // Resolvido
		db.MarkResolved(s.DBHumanID)
		s.State = StateDone
		msg := fmt.Sprintf(
			"✅ *Fico feliz que o problema foi resolvido, %s!*\n\n"+
				"Obrigado pelo contato com o "+config.C.Bot.SupportName+".\n\n"+
				"Envie uma nova mensagem para reiniciar o atendimento.",
			s.FirstName,
		)
		if s.GLPITicketID > 0 {
			link := glpi.SatisfactionLink(s.GLPITicketID)
			msg = fmt.Sprintf(
				"✅ *Fico feliz que o problema foi resolvido, %s!*\n\n"+
					"Que tal avaliar o atendimento? Sua opinião é muito importante:\n\n"+
					"📋 %s\n\n"+
					"Envie uma nova mensagem para reiniciar o atendimento.",
				s.FirstName, link,
			)
		}
		return txt(msg)

	case "2": // Não resolvido
		db.MarkUnresolved(s.DBHumanID)
		s.State = StateHumanWaiting
		s.HumanDeadline = time.Now().Add(time.Duration(config.C.Bot.HumanTimeoutMinutes) * time.Minute)
		// Notifica Teams com urgência
		displayName := s.Matricula
		if s.UserInfo != nil && s.UserInfo.DisplayName != "" {
			displayName = s.UserInfo.DisplayName
		}
		go teams.SendFollowUpAlert(displayName, s.Matricula, s.Phone, s.GLPITicketID)
		return txt(fmt.Sprintf(
			"😔 Entendido, *%s*. Já notificamos a equipe de suporte como urgência.\n\n"+
				"Um técnico entrará em contato em breve.\n\nUrgências: 📞 %s",
			s.FirstName, urgencyPhones(),
		))
	}
	return txt("Por favor, responda:\n\n1️⃣  Sim, problema resolvido\n2️⃣  Não, ainda preciso de ajuda")
}

// AttendantResponded é chamado pelo main quando detecta mensagem do atendente (IsFromMe=true, não do bot).
// Silencia o bot e registra quem atendeu.
func AttendantResponded(userJID, attendantPhone string) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()

	// Tenta match exato; se falhar busca pelo número ignorando sufixo
	// (@lid vs @s.whatsapp.net — WhatsApp usa JIDs diferentes para sent/received)
	s, ok := sessions[userJID]
	if !ok {
		userNum := strings.Split(userJID, "@")[0]
		for key, sess := range sessions {
			if strings.Split(key, "@")[0] == userNum {
				s = sess
				ok = true
				fmt.Printf("[BOT] AttendantResponded: JID remapeado %s → %s\n", userJID, key)
				break
			}
		}
	}
	if !ok {
		// Fora do horário: cria sessão mínima para silenciar o bot se o usuário responder.
		// Durante o horário, o atendente deve responder apenas dentro do fluxo normal.
		if isWorkingHours() {
			return
		}
		userNum := strings.Split(userJID, "@")[0]
		s = &Session{
			State:            StateAttended,
			JID:              userJID,
			Phone:            userNum,
			AttendantID:      attendantPhone,
			AttendantLastMsg: time.Now(),
			LastActivity:     time.Now(),
		}
		sessions[userJID] = s
		fmt.Printf("[BOT] Atendente assumiu fora de horário (sessão nova) para %s\n", userJID)
		return
	}

	// Se a sessão já está concluída, não reabrir
	if s.State == StateDone {
		return
	}

	now := time.Now()
	s.AttendantLastMsg = now

	if s.State == StateAttended {
		// Atendente continuando — só atualiza timestamp
		db.UpdateLastAttendantMsg(s.DBHumanID)
	} else if s.State == StateHumanWaiting {
		// Ticket aberto, atendente respondeu pela primeira vez
		s.State = StateAttended
		s.AttendantID = attendantPhone
		db.MarkAttended(s.DBHumanID, attendantPhone)
	} else if s.State == StateFollowUp {
		// Usuário recebeu pergunta de acompanhamento mas atendente voltou a responder
		s.State = StateAttended
		if s.AttendantID == "" {
			s.AttendantID = attendantPhone
		}
		db.UpdateLastAttendantMsg(s.DBHumanID)
	} else if !isWorkingHours() {
		// Fora do horário: atendente assumiu sessão em qualquer estado, silencia o bot
		oldState := s.State
		s.State = StateAttended
		s.AttendantID = attendantPhone
		if s.DBHumanID > 0 {
			db.MarkAttended(s.DBHumanID, attendantPhone)
		}
		fmt.Printf("[BOT] Atendente assumiu fora de horário (era estado %d) para %s\n", oldState, userJID)
	}
}

// ═══════════════════════════════════════════════════════════════
//  CHECKERS — chamados pelo main a cada minuto
// ═══════════════════════════════════════════════════════════════

// CheckHumanTimeouts verifica atendimentos sem resposta após HumanTimeoutMinutes.
// Notifica o Teams, avisa o usuário sobre alta demanda e fecha o registro no banco.
func CheckHumanTimeouts() map[string]string {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	alerts := make(map[string]string)
	for jid, s := range sessions {
		if s.State != StateHumanWaiting || !time.Now().After(s.HumanDeadline) {
			continue
		}
		s.State = StateDone

		// Fecha o registro no banco (finished_at) sem sobrescrever glpi_ticket_id
		if s.DBHumanID > 0 {
			db.CloseHumanRequest(s.DBHumanID)
		}

		waitMin := config.C.Bot.HumanTimeoutMinutes
		if waitMin == 0 {
			waitMin = 10
		}
		name := s.Matricula
		if s.UserInfo != nil && s.UserInfo.DisplayName != "" {
			name = s.UserInfo.DisplayName
		}
		go teams.SendWaitingAlert(name, s.Matricula, s.Phone, s.GLPITicketID, waitMin)

		var waitMsg string
		if s.GLPITicketID > 0 {
			waitMsg = fmt.Sprintf(
				"⚠️ *Estamos com alta demanda no momento.*\n\nSeu chamado *#%d* está registrado e será atendido em breve.\n\nUrgências: 📞 %s",
				s.GLPITicketID, urgencyPhones(),
			)
		} else {
			waitMsg = fmt.Sprintf(
				"⚠️ *Estamos com alta demanda no momento.*\n\nSeu chamado está registrado e será atendido em breve.\n\nUrgências: 📞 %s",
				urgencyPhones(),
			)
		}
		alerts[jid] = waitMsg
	}
	return alerts
}

// CheckAttendedTimeouts verifica sessões em StateAttended sem atividade do atendente por AttendantTimeoutMinutes.
// Quando expirado, envia pergunta de acompanhamento ao usuário.
func CheckAttendedTimeouts() map[string]string {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()

	followUps := make(map[string]string)
	attendantTimeout := config.C.Bot.AttendantTimeoutMinutes
	if attendantTimeout == 0 {
		attendantTimeout = 30
	}
	timeout := time.Duration(attendantTimeout) * time.Minute

	for jid, s := range sessions {
		if s.State != StateAttended {
			continue
		}
		if time.Since(s.AttendantLastMsg) < timeout {
			continue
		}
		s.State = StateFollowUp
		db.MarkFollowUpSent(s.DBHumanID)
		followUps[jid] = fmt.Sprintf(
			"Olá, *%s*! 👋\n\nPassou um tempo desde a última interação com nosso suporte.\n\n"+
				"O seu problema foi resolvido?\n\n"+
				"1️⃣  Sim, problema resolvido ✅\n"+
				"2️⃣  Não, ainda preciso de ajuda 🆘",
			s.FirstName,
		)
	}
	return followUps
}

// CheckInactivityTimeouts encerra sessões inativas por mais de X minutos.
func CheckInactivityTimeouts() map[string]string {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()

	timeout := time.Duration(config.C.Bot.InactivityTimeoutMinutes) * time.Minute
	expired := make(map[string]string)

	for jid, s := range sessions {
		// Estados com timers dedicados não são encerrados por inatividade
		switch s.State {
		case StateDone, StateGreeting, StateHumanWaiting, StateAttended, StateFollowUp:
			continue
		}
		if time.Since(s.LastActivity) <= timeout {
			continue
		}
		// Fecha registros abertos no banco
		if s.DBUnlockID > 0 && s.State == StateUnlockCode {
			db.FinishUnlockRequest(s.DBUnlockID, "expired_inactivity")
		}
		if s.DBFirewallID > 0 && s.State == StateFirewallCode {
			db.FinishFirewallRequest(s.DBFirewallID, 0, "expired_inactivity")
		}

		s.State = StateDone
		expired[jid] = fmt.Sprintf(
			"⏱️ *Atendimento encerrado por inatividade.*\n\nNão recebemos sua resposta em %d minutos.\n\nEnvie uma nova mensagem para reiniciar o atendimento.",
			config.C.Bot.InactivityTimeoutMinutes,
		)
	}
	return expired
}

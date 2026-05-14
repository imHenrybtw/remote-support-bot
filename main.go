package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/mdp/qrterminal/v3"

	"whatsapp-bot/bot"
	"whatsapp-bot/config"
	"whatsapp-bot/db"
	"whatsapp-bot/vpn"
)

var waClient *whatsmeow.Client

// ── Set de IDs de mensagens enviadas pelo bot ─────────────────────────────────
// Usado para distinguir mensagens do bot das mensagens do atendente
// (ambas chegam com IsFromMe=true).

type msgIDSet struct {
	mu  sync.Mutex
	ids map[string]time.Time // msgID → timestamp de envio
}

func newMsgIDSet() *msgIDSet {
	return &msgIDSet{ids: make(map[string]time.Time)}
}

func (s *msgIDSet) Add(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids[id] = time.Now()
}

func (s *msgIDSet) Has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.ids[id]
	return ok
}

// Purge remove IDs com mais de 10 minutos (limpeza periódica)
func (s *msgIDSet) Purge() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-10 * time.Minute)
	for id, t := range s.ids {
		if t.Before(cutoff) {
			delete(s.ids, id)
		}
	}
}

var botSentIDs = newMsgIDSet()

// ── Envio de mensagens ────────────────────────────────────────────────────────

func sendText(jid types.JID, text string) {
	fmt.Printf("[BOT → %s] %q\n", jid.User, text)
	resp, err := waClient.SendMessage(context.Background(), jid, &waProto.Message{
		Conversation: proto.String(text),
	})
	if err != nil {
		fmt.Printf("[ERRO] sendText: %v\n", err)
		return
	}
	botSentIDs.Add(resp.ID)
}

func sendImage(jid types.JID, imageFile, caption string) {
	data, err := vpn.ImageBytes(imageFile)
	if err != nil {
		fmt.Printf("[WARN] Imagem não encontrada: %s\n", imageFile)
		sendText(jid, caption)
		return
	}

	uploaded, err := waClient.Upload(context.Background(), data, whatsmeow.MediaImage)
	if err != nil {
		fmt.Printf("[WARN] Falha no upload da imagem %s: %v\n", imageFile, err)
		sendText(jid, caption)
		return
	}

	fmt.Printf("[BOT → %s] 🖼️  %s | %q\n", jid.User, imageFile, caption)
	resp, err := waClient.SendMessage(context.Background(), jid, &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			Caption:       proto.String(caption),
			Mimetype:      proto.String("image/png"),
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uploaded.FileLength),
		},
	})
	if err != nil {
		fmt.Printf("[ERRO] sendImage: %v\n", err)
		sendText(jid, caption)
		return
	}
	botSentIDs.Add(resp.ID)
}

func dispatchResponse(jid types.JID, response bot.Response) {
	for _, msg := range response.Messages {
		switch msg.Type {
		case bot.MsgImage:
			sendImage(jid, msg.ImageFile, msg.Text)
		default:
			if msg.Text != "" {
				sendText(jid, msg.Text)
			}
		}
	}
}

// ── Whitelist ────────────────────────────────────────────────────────────────

// isAuthorized consulta a tabela allowed_phones para verificar se o número
// está cadastrado como funcionário home office autorizado.
// Fail-closed: em caso de erro no banco, bloqueia por segurança.
func isAuthorized(phone string) bool {
	allowed, err := db.IsPhoneAllowed(phone)
	if err != nil {
		fmt.Printf("[WARN] Erro ao verificar whitelist para %s: %v — bloqueando por segurança.\n", phone, err)
		return false
	}
	return allowed
}

// ── Handler de eventos ────────────────────────────────────────────────────────

func reconnectWithBackoff() {
	backoff := 5 * time.Second
	const maxBackoff = 5 * time.Minute
	for {
		time.Sleep(backoff)
		fmt.Printf("[BOT] Reconectando ao WhatsApp...\n")
		if err := waClient.Connect(); err != nil {
			fmt.Printf("[ERRO] Reconexão falhou: %v. Próxima tentativa em %s\n", err, backoff*2)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		} else {
			fmt.Println("[BOT] Reconectado com sucesso!")
			return
		}
	}
}

func eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Disconnected:
		fmt.Println("[WARN] WhatsApp desconectado — tentando reconectar...")
		go reconnectWithBackoff()
	case *events.StreamReplaced:
		fmt.Println("[WARN] Stream substituído — reconectando...")
		go reconnectWithBackoff()
	case *events.LoggedOut:
		fmt.Printf("[ERRO] Sessão encerrada pelo servidor (razão: %v). Novo QR necessário.\n", v.Reason)
	case *events.Message:
		if v.Info.IsGroup {
			return
		}

		if v.Info.IsFromMe {
			// Verifica se foi o bot ou o atendente que enviou
			if botSentIDs.Has(v.Info.ID) {
				return // mensagem do próprio bot — ignora
			}
			// IsFromMe=true mas não está no set → foi o atendente
			userJID := v.Info.Chat.String()
			attendantPhone := waClient.Store.ID.User
			fmt.Printf("[ATENDENTE] %s respondeu para %s\n", attendantPhone, userJID)
			bot.AttendantResponded(userJID, attendantPhone)
			return
		}

		// Resolve o número real de telefone
		phone := v.Info.Chat.User
		if v.Info.Chat.Server == types.HiddenUserServer {
			// JID LID: Chat.User é opaco; busca o JID de telefone nos campos alternativos
			for _, alt := range []types.JID{v.Info.SenderAlt, v.Info.Sender, v.Info.RecipientAlt} {
				if alt.Server == types.DefaultUserServer && alt.User != "" {
					phone = alt.User
					break
				}
			}
		}

		// ── Verificação de whitelist ──────────────────────────────────────
		// Apenas funcionários cadastrados como home office podem usar o bot.
		// Números não autorizados são descartados silenciosamente (sem resposta),
		// evitando confirmar a existência do serviço para terceiros.
		if !isAuthorized(phone) {
			fmt.Printf("[ACESSO NEGADO] %s não está na whitelist — descartado.\n", phone)
			return
		}
		// ─────────────────────────────────────────────────────────────────

		var text string
		if v.Message.GetConversation() != "" {
			text = v.Message.GetConversation()
		} else if v.Message.GetExtendedTextMessage() != nil {
			text = v.Message.GetExtendedTextMessage().GetText()
		}

		if strings.TrimSpace(text) == "" {
			return
		}

		senderJID := v.Info.Chat.String()
		fmt.Printf("[MSG] %s → %q\n", phone, text)

		response := bot.Handle(senderJID, phone, text)
		dispatchResponse(v.Info.Chat, response)
	}
}

// ── Verificadores periódicos ──────────────────────────────────────────────────

func startCheckers() {
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		defer ticker.Stop()
		for range ticker.C {
			botSentIDs.Purge()
			bot.PurgeRateLimiter()

			for jidStr, msg := range bot.CheckHumanTimeouts() {
				if parsed, err := types.ParseJID(jidStr); err == nil {
					sendText(parsed, msg)
				}
			}

			for jidStr, msg := range bot.CheckAttendedTimeouts() {
				if parsed, err := types.ParseJID(jidStr); err == nil {
					sendText(parsed, msg)
				}
			}

			for jidStr, msg := range bot.CheckInactivityTimeouts() {
				if parsed, err := types.ParseJID(jidStr); err == nil {
					sendText(parsed, msg)
				}
			}
		}
	}()
}

// ── Main ─────────────────────────────────────────────────────────────────────

func main() {
	cfgPath := "config.yaml"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	// 1. Carrega configuração e segredos do .env
	config.Load(cfgPath)

	// 2. Reconstrói o guia de VPN com os valores do config
	//    (gateway, porta e nome do perfil vêm do config.yaml)
	vpn.RebuildGuide()

	// 3. Conecta ao PostgreSQL e executa migrations
	db.Connect()
	db.CleanupPendingOnStartup()

	// 4. Inicializa cliente WhatsApp
	dbLog := waLog.Stdout("Database", "WARN", true)
	container, err := sqlstore.New(context.Background(), "sqlite3", "file:wa-sessions.db?_foreign_keys=on", dbLog)
	if err != nil {
		panic(err)
	}
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		panic(err)
	}

	clientLog := waLog.Stdout("Client", "WARN", true)
	waClient = whatsmeow.NewClient(deviceStore, clientLog)
	waClient.AddEventHandler(eventHandler)

	if waClient.Store.ID == nil {
		// Primeira execução — exibe QR Code para autenticação
		qrChan, _ := waClient.GetQRChannel(context.Background())
		if err = waClient.Connect(); err != nil {
			panic(err)
		}
		fmt.Println("\n📱 Escaneie o QR Code com o WhatsApp:\n")
		for evt := range qrChan {
			if evt.Event == "code" {
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else {
				fmt.Printf("QR: %s\n", evt.Event)
			}
		}
	} else {
		if err = waClient.Connect(); err != nil {
			panic(err)
		}
		fmt.Println("✅ Bot reconectado com sessão existente!")
	}

	startCheckers()

	fmt.Println("🤖 Bot de suporte rodando. Ctrl+C para encerrar.")
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	waClient.Disconnect()
	fmt.Println("\n👋 Encerrado.")
}

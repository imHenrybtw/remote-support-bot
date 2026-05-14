package vpn

import (
	"fmt"
	"os"
	"path/filepath"
	"whatsapp-bot/config"
)

// Step representa um passo do guia de VPN.
type Step struct {
	Text      string
	ImageFile string // nome do arquivo dentro de config.VPN.ImagesPath
}

// Guide contém os passos do tutorial enviados ao usuário.
var Guide []Step

func RebuildGuide() {
	cfg := config.C.VPN

	Guide = []Step{
		{
			Text: `2️⃣ *Abrir o FortiClient VPN*

Ao abrir, você verá a tela inicial. Clique em *"VPN Remota"* no painel lateral esquerdo.`,
			ImageFile: "02-tela-inicial.png",
		},
		{
			Text: fmt.Sprintf(`3️⃣ *Verificar o perfil de conexão*

Confirme que o perfil exibido é *%s* com as seguintes configurações:
• Remote Gateway: %s
• ✅ Customize Port: %d
• Protocolo: SSL-VPN

Se o perfil não existir, clique em *"+"* para adicionar.`,
				cfg.ProfileName, cfg.Gateway, cfg.Port),
			ImageFile: "03-perfil-vpn.png",
		},
		{
			Text: `4️⃣ *Conectar à VPN*

Clique em *Conectar*, informe:
• Usuário: sua matrícula
• Senha: senha da rede corporativa

⚠️ Se aparecer aviso de certificado, clique em *Continuar* — é esperado.`,
			ImageFile: "04-login-vpn.png",
		},
		{
			Text: `5️⃣ *Verificar status da conexão*

Após conectar:
✅ O ícone na bandeja do sistema fica *verde*
✅ O status exibe *"Conectado"*

A VPN está funcionando! 🎉`,
			ImageFile: "05-conectado.png",
		},
	}
}

// ImagePath retorna o caminho absoluto de um arquivo de imagem do guia.
func ImagePath(filename string) string {
	return filepath.Join(config.C.VPN.ImagesPath, filename)
}

// ImageExists verifica se o arquivo de imagem existe no disco.
func ImageExists(filename string) bool {
	_, err := os.Stat(ImagePath(filename))
	return err == nil
}

// ImageBytes lê e retorna os bytes de uma imagem do guia.
func ImageBytes(filename string) ([]byte, error) {
	path := ImagePath(filename)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("imagem não encontrada: %s", path)
	}
	return data, nil
}

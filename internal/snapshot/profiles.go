package snapshot

var ChatGPTRels = []string{
	"Cookies",
	"Cookies-journal",
	"Login Data",
	"Login Data For Account",
	"Session Storage",
	"WebStorage",
}

var GrokBotRels = []string{
	"Cookies",
	"Local Storage",
	"Session Storage",
	"sand-session-marker.json",
	"Partitions/sand-forever-box/Cookies",
	"Partitions/sand-forever-box/Local Storage",
	"Partitions/sand-forever-box/Session Storage",
}

const (
	ChatGPTRelRoot = "Library/Application Support/Codex/Default"
	GrokBotRelRoot = "Library/Application Support/Grok Bot"
	KindDesktop    = "desktop.profile.v1"
)

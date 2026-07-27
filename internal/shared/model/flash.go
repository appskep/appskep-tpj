package model

// Flash levels, matching the variants the alert partial knows how to style.
const (
	FlashLevelSuccess = "success"
	FlashLevelError   = "error"
	FlashLevelWarning = "warning"
	FlashLevelInfo    = "info"
)

// Flash is a one-shot message carried across a redirect in the session cookie
// and rendered above the page content by the layout.
//
// It lives in model rather than view because the session codec has to serialise
// it: view imports model, and a Flash defined in view would make model import
// view right back.
type Flash struct {
	Level   string `json:"l"`
	Message string `json:"m"`
}

// FlashSuccess and friends are the constructors used at redirect sites, so no
// caller has to remember the level strings.
func FlashSuccess(msg string) Flash { return Flash{Level: FlashLevelSuccess, Message: msg} }
func FlashError(msg string) Flash   { return Flash{Level: FlashLevelError, Message: msg} }
func FlashWarning(msg string) Flash { return Flash{Level: FlashLevelWarning, Message: msg} }
func FlashInfo(msg string) Flash    { return Flash{Level: FlashLevelInfo, Message: msg} }

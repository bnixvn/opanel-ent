package apache

import (
	"strings"
	"testing"
)

// LiteSpeed writes httpd_config.xml as very nearly one long line. The add
// path used to anchor on an element at the start of a line, matched nothing,
// and returned the document untouched -- so a setting the panel believed it
// had written was simply absent, and the only symptom was PHP running as the
// wrong user.
func TestSetXMLValueAddsToASingleLineDocument(t *testing.T) {
	doc := []byte(`<?xml version="1.0"?><httpServerConfig><user>nobody</user>` +
		`<group>nobody</group><enableLVE>2</enableLVE></httpServerConfig>`)

	out := string(setXMLValue(doc, "phpSuExec", "1"))
	if !strings.Contains(out, "<phpSuExec>1</phpSuExec>") {
		t.Fatalf("the setting was not added:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "</httpServerConfig>") {
		t.Errorf("it landed outside the root element:\n%s", out)
	}
	if strings.Contains(out, "</httpServerConfig></httpServerConfig>") {
		t.Errorf("the root element was duplicated:\n%s", out)
	}
}

// Replacing an existing value must not add a second copy.
func TestSetXMLValueReplacesInPlace(t *testing.T) {
	doc := []byte(`<httpServerConfig><phpSuExec>0</phpSuExec></httpServerConfig>`)
	out := string(setXMLValue(doc, "phpSuExec", "1"))
	if strings.Count(out, "<phpSuExec>") != 1 {
		t.Errorf("phpSuExec appears %d times:\n%s", strings.Count(out, "<phpSuExec>"), out)
	}
	if !strings.Contains(out, "<phpSuExec>1</phpSuExec>") {
		t.Errorf("the value was not replaced:\n%s", out)
	}
}

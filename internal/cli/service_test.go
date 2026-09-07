package cli

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func TestBuildServicePlistUsesCaffeinateAndAbsoluteBinary(t *testing.T) {
	plist := buildServicePlist(servicePlistOptions{
		Executable:   "/Users/test/My Tools/limitping",
		Provider:     "claude",
		PreventSleep: true,
		Path:         "/opt/homebrew/bin:/usr/bin",
		Home:         "/Users/test",
		StdoutPath:   "/Users/test/service.log",
		StderrPath:   "/Users/test/service.error.log",
	})
	for _, want := range []string{
		"<string>/usr/bin/caffeinate</string>",
		"<string>-s</string>",
		"<string>/Users/test/My Tools/limitping</string>",
		"<string>watch</string>",
		"<string>claude</string>",
		"<key>KeepAlive</key>",
		"<key>RunAtLoad</key>",
		"<key>WorkingDirectory</key>",
		"<string>/Users/test</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
	decoder := xml.NewDecoder(strings.NewReader(plist))
	for {
		if _, err := decoder.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("plist is not valid XML: %v\n%s", err, plist)
		}
	}
}

func TestBuildServicePlistEscapesValues(t *testing.T) {
	plist := buildServicePlist(servicePlistOptions{
		Executable: "/tmp/a&b<limitping>", Provider: "claude",
		Path: "/a&b", Home: "/Users/test", StdoutPath: "/tmp/out", StderrPath: "/tmp/err",
	})
	if strings.Contains(plist, "a&b<limitping>") || !strings.Contains(plist, "a&amp;b&lt;limitping&gt;") {
		t.Fatalf("plist did not XML-escape values:\n%s", plist)
	}
}

func TestBuildServicePlistCanAllowSleep(t *testing.T) {
	plist := buildServicePlist(servicePlistOptions{Executable: "/bin/limitping", Provider: "claude"})
	if strings.Contains(plist, "caffeinate") {
		t.Fatalf("plist unexpectedly contains caffeinate:\n%s", plist)
	}
}

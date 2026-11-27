package hotspot

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kel077/pith/internal/api"
)

func TestIdentifyInseegoStatus(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		want       api.ErrorCode
	}{
		{"connected", `{"loginStatus":0,"statusData":{"statusBarConnectionState":"Connected","statusBarTechnology":"5G","statusBarSignalBars":4}}`, 200, ""},
		{"offline", `{"loginStatus":"0","statusData":{"statusBarConnectionState":"Ready","statusBarConnectionDuration":"0","statusBarSignalBars":"0"}}`, 200, ""},
		{"HTML", "<html>Example router</html>", 200, api.ErrUnsupported},
		{"other JSON", `{"status":"connected","model":"M2000"}`, 200, api.ErrUnsupported},
		{"missing login and signal", `{"statusData":{"statusBarConnectionState":"Connected","statusBarTechnology":"5G"}}`, 200, api.ErrUnsupported},
		{"missing endpoint", "not found", 404, api.ErrUnsupported},
		{"temporary failure", "try again", 503, api.ErrInternal},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/srv/status" || r.Header.Get("Cookie") != "" {
					t.Errorf("identification sent %s %s with cookie %q", r.Method, r.URL.Path, r.Header.Get("Cookie"))
				}
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			d := Inseego{Base: server.URL, Model: "M2000", HTTP: server.Client(), Password: func() (string, error) {
				t.Fatal("identification asked for a password")
				return "", nil
			}}
			if err := d.Identify(t.Context()); api.Code(err) != test.want {
				t.Fatalf("Identify = %v, want %q", err, test.want)
			}
		})
	}
}

package gothic_test

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"html"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bgdsh/goth"
	. "github.com/bgdsh/goth/gothic"
	"github.com/bgdsh/goth/providers/faux"
	"github.com/gorilla/sessions"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
)

type mapKey struct {
	r *http.Request
	n string
}

type ProviderStore struct {
	Store map[mapKey]*sessions.Session
}

func NewProviderStore() *ProviderStore {
	return &ProviderStore{map[mapKey]*sessions.Session{}}
}

func (p ProviderStore) Get(r *http.Request, name string) (*sessions.Session, error) {
	s := p.Store[mapKey{r, name}]
	if s == nil {
		s, err := p.New(r, name)
		return s, err
	}
	return s, nil
}

func (p ProviderStore) New(r *http.Request, name string) (*sessions.Session, error) {
	s := sessions.NewSession(p, name)
	s.Options = &sessions.Options{
		Path:   "/",
		MaxAge: 86400 * 30,
	}
	p.Store[mapKey{r, name}] = s
	return s, nil
}

func (p ProviderStore) Save(r *http.Request, w http.ResponseWriter, s *sessions.Session) error {
	p.Store[mapKey{r, s.Name()}] = s
	return nil
}

var fauxProvider goth.Provider

func Test_BeginAuthHandler(t *testing.T) {
	a := assert.New(t)

	res := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/auth?provider=faux", nil)
	a.NoError(err)

	c := echo.New().NewContext(req, res)
	BeginAuthHandler(c)

	sess, err := session.Get(SessionName, c)
	if err != nil {
		t.Fatalf("error getting faux Gothic session: %v", err)
	}

	sessStr, ok := sess.Values["faux"].(string)
	if !ok {
		t.Fatalf("Gothic session not stored as marshalled string; was %T (value %v)",
			sess.Values["faux"], sess.Values["faux"])
	}
	gothSession, err := fauxProvider.UnmarshalSession(ungzipString(sessStr))
	if err != nil {
		t.Fatalf("error unmarshalling faux Gothic session: %v", err)
	}
	au, _ := gothSession.GetAuthURL()

	a.Equal(http.StatusTemporaryRedirect, res.Code)
	a.Contains(res.Body.String(),
		fmt.Sprintf(`<a href="%s">Temporary Redirect</a>`, html.EscapeString(au)))
}

func Test_GetAuthURL(t *testing.T) {
	a := assert.New(t)

	res := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/auth?provider=faux", nil)
	a.NoError(err)

	c := echo.New().NewContext(req, res)

	u, err := GetAuthURL(c)
	a.NoError(err)

	// Check that we get the correct auth URL with a state parameter
	parsed, err := url.Parse(u)
	a.NoError(err)
	a.Equal("http", parsed.Scheme)
	a.Equal("example.com", parsed.Host)
	q := parsed.Query()
	a.Contains(q, "client_id")
	a.Equal("code", q.Get("response_type"))
	a.NotZero(q, "state")

	// Check that if we run GetAuthURL on another request, that request's
	// auth URL has a different state from the previous one.
	req2, err := http.NewRequest("GET", "/auth?provider=faux", nil)
	a.NoError(err)

	c = echo.New().NewContext(req2, httptest.NewRecorder())
	url2, err := GetAuthURL(c)
	a.NoError(err)
	parsed2, err := url.Parse(url2)
	a.NoError(err)
	a.NotEqual(parsed.Query().Get("state"), parsed2.Query().Get("state"))
}

func Test_CompleteUserAuth(t *testing.T) {
	a := assert.New(t)

	res := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/auth/callback?provider=faux", nil)
	a.NoError(err)

	sess := faux.Session{Name: "Homer Simpson", Email: "homer@example.com"}
	c := echo.New().NewContext(req, res)
	session, _ := session.Get(SessionName, c)
	session.Values["faux"] = gzipString(sess.Marshal())
	err = session.Save(req, res)
	a.NoError(err)

	user, err := CompleteUserAuth(c)
	a.NoError(err)

	a.Equal(user.Name, "Homer Simpson")
	a.Equal(user.Email, "homer@example.com")
}

func Test_CompleteUserAuthWithSessionDeducedProvider(t *testing.T) {
	a := assert.New(t)

	res := httptest.NewRecorder()
	// Inteintionally omit a provider argument, force looking in session.
	req, err := http.NewRequest("GET", "/auth/callback", nil)
	a.NoError(err)

	sess := faux.Session{Name: "Homer Simpson", Email: "homer@example.com"}
	c := echo.New().NewContext(req, res)

	session, _ := session.Get(SessionName, c)
	session.Values["faux"] = gzipString(sess.Marshal())
	err = session.Save(req, res)
	a.NoError(err)
	user, err := CompleteUserAuth(c)
	a.NoError(err)

	a.Equal(user.Name, "Homer Simpson")
	a.Equal(user.Email, "homer@example.com")
}

func Test_SetState(t *testing.T) {
	a := assert.New(t)

	req, _ := http.NewRequest("GET", "/auth?state=state", nil)
	c := echo.New().NewContext(req, nil)
	a.Equal(SetState(c), "state")
}

func Test_GetState(t *testing.T) {
	a := assert.New(t)

	req, _ := http.NewRequest("GET", "/auth?state=state", nil)
	c := echo.New().NewContext(req, nil)
	a.Equal(GetState(c), "state")
}

func Test_AppleStateValidation(t *testing.T) {
	a := assert.New(t)
	appleStateValue := "xyz123-#"
	form := url.Values{}
	form.Add("state", appleStateValue)
	req, _ := http.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Form = form
	c := echo.New().NewContext(req, nil)
	a.Equal(appleStateValue, GetState(c))
}

func gzipString(value string) string {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := gz.Write([]byte(value)); err != nil {
		return "err"
	}
	if err := gz.Flush(); err != nil {
		return "err"
	}
	if err := gz.Close(); err != nil {
		return "err"
	}

	return b.String()
}

func ungzipString(value string) string {
	rdata := strings.NewReader(value)
	r, err := gzip.NewReader(rdata)
	if err != nil {
		return "err"
	}
	s, err := ioutil.ReadAll(r)
	if err != nil {
		return "err"
	}

	return string(s)
}

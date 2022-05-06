/*
Package gothic wraps common behaviour when using goth. This makes it quick, and easy, to get up
and running with goth. Of course, if you want complete control over how things flow, in regards
to the authentication process, feel free and use Goth directly.

See https://github.com/bgdsh/goth/blob/master/examples/main.go to see this in action.
*/
package gothic

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/bgdsh/goth"
	"github.com/gorilla/sessions"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
)

// SessionName is the key used to access the session store.
const SessionName = "_gothic_session"

type key int

// ProviderParamKey can be used as a key in context when passing in a provider
const ProviderParamKey key = iota

/*
BeginAuthHandler is a convenience handler for starting the authentication process.
It expects to be able to get the name of the provider from the query parameters
as either "provider" or ":provider".

BeginAuthHandler will redirect the user to the appropriate authentication end-point
for the requested provider.

See https://github.com/bgdsh/goth/examples/main.go to see this in action.
*/
func BeginAuthHandler(c echo.Context) error {
	authUrl, err := GetAuthURL(c)
	if err != nil {
		c.Logger().Error(err)
		return c.String(http.StatusBadRequest, err.Error())
	}
	return c.Redirect(http.StatusTemporaryRedirect, authUrl)
}

// SetState sets the state string associated with the given request.
// If no state string is associated with the request, one will be generated.
// This state is sent to the provider and can be retrieved during the
// callback.
var SetState = func(c echo.Context) string {
	state := c.QueryParam("state")
	if len(state) > 0 {
		return state
	}

	// If a state query param is not passed in, generate a random
	// base64-encoded nonce so that the state on the auth URL
	// is unguessable, preventing CSRF attacks, as described in
	//
	// https://auth0.com/docs/protocols/oauth2/oauth-state#keep-reading
	nonceBytes := make([]byte, 64)
	_, err := io.ReadFull(rand.Reader, nonceBytes)
	if err != nil {
		panic("gothic: source of randomness unavailable: " + err.Error())
	}
	return base64.URLEncoding.EncodeToString(nonceBytes)
}

// GetState gets the state returned by the provider during the callback.
// This is used to prevent CSRF attacks, see
// http://tools.ietf.org/html/rfc6749#section-10.12
var GetState = func(c echo.Context) string {
	if c.QueryParams().Encode() == "" && c.Request().Method == http.MethodPost {
		return c.FormValue("state")
	}
	return c.QueryParam("state")
}

/*
GetAuthURL starts the authentication process with the requested provided.
It will return a URL that should be used to send users to.

It expects to be able to get the name of the provider from the query parameters
as either "provider" or ":provider".

I would recommend using the BeginAuthHandler instead of doing all of these steps
yourself, but that's entirely up to you.
*/
func GetAuthURL(c echo.Context) (string, error) {
	providerName, err := GetProviderName(c)
	if err != nil {
		return "", err
	}

	provider, err := goth.GetProvider(providerName)
	if err != nil {
		return "", err
	}
	sess, err := provider.BeginAuth(SetState(c))
	log.Println(sess.Marshal())
	if err != nil {
		return "", err
	}

	authUrl, err := sess.GetAuthURL()
	if err != nil {
		return "", err
	}

	err = StoreInSession(providerName, sess.Marshal(), c)

	if err != nil {
		return "", err
	}

	return authUrl, err
}

/*
CompleteUserAuth does what it says on the tin. It completes the authentication
process and fetches all of the basic information about the user from the provider.

It expects to be able to get the name of the provider from the query parameters
as either "provider" or ":provider".

See https://github.com/bgdsh/goth/examples/main.go to see this in action.
*/
var CompleteUserAuth = func(c echo.Context) (goth.User, error) {

	providerName, err := GetProviderName(c)
	if err != nil {
		return goth.User{}, err
	}

	provider, err := goth.GetProvider(providerName)
	if err != nil {
		return goth.User{}, err
	}

	value, err := GetFromSession(providerName, c)
	if err != nil {
		return goth.User{}, err
	}
	defer Logout(c) // clear the google auth session
	sess, err := provider.UnmarshalSession(value)
	if err != nil {
		return goth.User{}, err
	}

	err = validateState(c, sess)
	if err != nil {
		return goth.User{}, err
	}

	user, err := provider.FetchUser(sess)
	if err == nil {
		// user can be found with existing session data
		return user, err
	}

	params := c.QueryParams()
	if params.Encode() == "" && c.Request().Method == "POST" {
		params, err = c.FormParams()
		if err != nil {
			return goth.User{}, err
		}
	}

	// get new token and retry fetch
	_, err = sess.Authorize(provider, params)
	if err != nil {
		return goth.User{}, err
	}

	err = StoreInSession(providerName, sess.Marshal(), c)

	if err != nil {
		return goth.User{}, err
	}

	gu, err := provider.FetchUser(sess)
	return gu, err
}

// validateState ensures that the state token param from the original
// AuthURL matches the one included in the current (callback) request.
func validateState(c echo.Context, sess goth.Session) error {
	rawAuthURL, err := sess.GetAuthURL()
	if err != nil {
		return err
	}

	authURL, err := url.Parse(rawAuthURL)
	if err != nil {
		return err
	}

	reqState := GetState(c)

	originalState := authURL.Query().Get("state")
	if originalState != "" && (originalState != reqState) {
		return errors.New("state token mismatch")
	}
	return nil
}

// Logout invalidates a user session.
func Logout(c echo.Context) error {
	log.Println("Logout")
	sess, err := session.Get(SessionName, c)
	if err != nil {
		return err
	}
	sess.Options.MaxAge = -1
	sess.Values = make(map[interface{}]interface{})

	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   100, // if auth does not finish within 100 seconds, clear it
		HttpOnly: true,
	}

	err = sess.Save(c.Request(), c.Response())
	if err != nil {
		return errors.New("could not delete user session ")
	}
	return nil
}

// GetProviderName is a function used to get the name of a provider
// for a given request. By default, this provider is fetched from
// the URL query string. If you provide it in a different way,
// assign your own function to this variable that returns the provider
// name for your request.
var GetProviderName = getProviderName

func getProviderName(c echo.Context) (string, error) {

	// try to get it from the url param "provider"
	if p := c.Param("provider"); p != "" {
		return p, nil
	}

	// try to get it from the url param ":provider"
	if p := c.QueryParam(":provider"); p != "" {
		return p, nil
	}

	// // try to get it from the context's value of "provider" key
	// if p, ok := mux.Vars(req)["provider"]; ok {
	// 	return p, nil
	// }

	// //  try to get it from the go-context's value of "provider" key
	// if p, ok := req.Context().Value("provider").(string); ok {
	// 	return p, nil
	// }

	// // try to get it from the go-context's value of providerContextKey key
	// if p, ok := req.Context().Value(ProviderParamKey).(string); ok {
	// 	return p, nil
	// }

	// As a fallback, loop over the used providers, if we already have a valid session for any provider (ie. user has already begun authentication with a provider), then return that provider name
	providers := goth.GetProviders()
	sess, _ := session.Get(SessionName, c)
	for _, provider := range providers {
		p := provider.Name()
		value := sess.Values[p]
		if _, ok := value.(string); ok {
			return p, nil
		}
	}

	// if not found then return an empty string with the corresponding error
	return "", errors.New("you must select a provider")
}

// GetContextWithProvider returns a new request context containing the provider
func GetContextWithProvider(req *http.Request, provider string) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ProviderParamKey, provider))
}

// StoreInSession stores a specified key/value pair in the session.
func StoreInSession(key string, value string, c echo.Context) error {
	sess, _ := session.Get(SessionName, c)

	if err := updateSessionValue(sess, key, value); err != nil {
		return err
	}

	err := sess.Save(c.Request(), c.Response())

	return err
}

// GetFromSession retrieves a previously-stored value from the session.
// If no value has previously been stored at the specified key, it will return an error.
func GetFromSession(key string, c echo.Context) (string, error) {
	sess, _ := session.Get(SessionName, c)
	value, err := getSessionValue(sess, key)
	if err != nil {
		return "", errors.New("could not find a matching session for this request")
	}

	return value, nil
}

func getSessionValue(sess *sessions.Session, key string) (string, error) {
	value := sess.Values[key]
	if value == nil {
		return "", fmt.Errorf("could not find a matching session for this request")
	}
	rdata := strings.NewReader(value.(string))
	r, err := gzip.NewReader(rdata)
	if err != nil {
		return "", err
	}
	s, err := ioutil.ReadAll(r)
	if err != nil {
		return "", err
	}
	return string(s), nil
}

func updateSessionValue(session *sessions.Session, key, value string) error {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := gz.Write([]byte(value)); err != nil {
		return err
	}
	if err := gz.Flush(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}

	session.Values[key] = b.String()
	return nil
}

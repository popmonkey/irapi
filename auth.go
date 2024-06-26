package irdata

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/gob"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const loginURL = "https://members-ng.iracing.com/auth"
const testUrl = "https://members-ng.iracing.com/data/constants/event_types"

type authDataT struct {
	Username        string
	EncodedPassword string
}

var additionalContext = []byte("irdata.auth")

// AuthWithCredsFromFile loads the username and password from a file
// at authFilename and encrypted with the key in keyFilename.
func (i *Irdata) AuthWithCredsFromFile(keyFilename string, authFilename string) error {
	authData := readCreds(keyFilename, authFilename)

	return i.auth(authData)
}

// AuthWithProvideCreds calls the provided function for the username and password
func (i *Irdata) AuthWithProvideCreds(authSource CredsProvider) error {
	log.WithFields(log.Fields{"authSource": authSource}).Debug("Calling CredsProvider")

	username, password := authSource.GetCreds()

	var authData authDataT

	authData.Username = string(username)
	authData.EncodedPassword = encodePassword(username, password)

	return i.auth(authData)
}

// SaveProvidedCredsToFile calls the provided function for the
// username and password and then saves these credentials to authFilename
// using the key within the keyFilename
//
// This function will panic out on errors
func SaveProvidedCredsToFile(keyFilename string, authFilename string, authSource CredsProvider) {
	log.WithFields(log.Fields{"authSource": authSource}).Debug("Calling CredsProvider")

	username, password := authSource.GetCreds()

	var authData authDataT

	authData.Username = string(username)
	authData.EncodedPassword = encodePassword(username, password)

	writeCreds(keyFilename, authFilename, authData)
}

func writeCreds(keyFilename string, authFilename string, authData authDataT) {
	key := getKey(keyFilename)

	block, err := aes.NewCipher(key)

	// not a defer because we want to do this right away
	shred(&key)

	if err != nil {
		if errors.Is(err, aes.KeySizeError(0)) {
			log.Panic(errors.New("key must be 16, 24, or 32 bytes long"))
		} else {
			log.Panic(err)
		}
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		log.Panic(err)
	}

	nonce, err := makeNonce(aesgcm)
	if err != nil {
		log.Panic(err)
	}

	buf := bytes.Buffer{}

	enc := gob.NewEncoder(&buf)

	err = enc.Encode(authData)
	if err != nil {
		log.Panic(err)
	}

	data := aesgcm.Seal(nonce, nonce, buf.Bytes(), additionalContext)

	base64data := base64.StdEncoding.Strict().EncodeToString(data)

	if err := os.WriteFile(authFilename, []byte(base64data), os.ModePerm); err != nil {
		log.Panic(err)
	}
}

func readCreds(keyFilename string, authFilename string) authDataT {
	key := getKey(keyFilename)

	block, err := aes.NewCipher(key)

	// not a defer because we want to do this right away
	shred(&key)

	if err != nil {
		log.Panic(err)
	}

	aesgcm, err := cipher.NewGCM(block)

	if err != nil {
		log.Panic(err)
	}

	var authData authDataT

	base64data, err := os.ReadFile(authFilename)
	if err != nil {
		log.Panic(err)
	}

	data, err := base64.StdEncoding.Strict().DecodeString(string(base64data))
	if err != nil {
		log.Panic(err)
	}

	authGob, err := aesgcm.Open(nil, data[:aesgcm.NonceSize()], data[aesgcm.NonceSize():], additionalContext)
	if err != nil {
		log.Panic(err)
	}

	buf := bytes.NewReader(authGob)

	dec := gob.NewDecoder(buf)

	err = dec.Decode(&authData)
	if err != nil {
		log.Panic(err)
	}

	return authData
}

// auth client
func (i *Irdata) auth(authData authDataT) error {
	if i.isAuthed {
		return nil
	}

	if authData.EncodedPassword == "" {
		return errors.New("must provide credentials before calling")
	}

	log.Info("Authenticating")

	retries := 5

	var err error
	var resp *http.Response

	for retries > 0 {
		resp, err = i.httpClient.Post(loginURL, "application/json",
			strings.NewReader(
				fmt.Sprintf("{\"email\": \"%s\" ,\"password\": \"%s\"}", authData.Username, authData.EncodedPassword),
			),
		)

		if resp.StatusCode < 500 {
			break
		}

		log.WithFields(log.Fields{"resp.StatusCode": resp.StatusCode}).Debug(" *** Retrying Authentication due to error")

		retries--

		time.Sleep(time.Duration((6-retries)*5) * time.Second)
	}

	if err != nil {
		log.Panic(err)
	}

	if resp.StatusCode != 200 {
		log.WithFields(log.Fields{
			"resp.Status":     resp.Status,
			"resp.StatusCode": resp.StatusCode,
		}).Info("Failed to authenticate")

		return errors.New("unexpected auth failure, try debug")
	}

	// test we are really auth'ed
	resp, err = i.retryingGet(testUrl)
	if err != nil {
		log.Panic(err)
	}

	if resp.StatusCode != 200 {
		if resp.StatusCode == 401 {
			return errors.New("login failed, check creds")
		} else {
			log.WithFields(log.Fields{
				"resp.Status":     resp.Status,
				"resp.StatusCode": resp.StatusCode,
				"testUrl":         testUrl,
			}).Info("Unexpected status")

			return errors.New("unexpected auth failure, try debug")
		}
	}

	log.Info("Login succeeded")

	i.isAuthed = true

	return nil
}

// See: https://forums.iracing.com/discussion/22109/login-form-changes/p1
func encodePassword(username []byte, password []byte) string {
	hasher := sha256.New()

	_, err := hasher.Write(password)
	if err != nil {
		log.Panic(err)
	}

	_, err = hasher.Write([]byte(strings.ToLower(string(username))))
	if err != nil {
		log.Panic(err)
	}

	return base64.StdEncoding.Strict().EncodeToString(hasher.Sum(nil))
}

// nonce generator
func makeNonce(gcm cipher.AEAD) ([]byte, error) {
	nonce := make([]byte, gcm.NonceSize())

	_, err := rand.Read(nonce)

	return nonce, err
}

// read secret key
func getKey(keyFilename string) []byte {
	stat, err := os.Stat(keyFilename)

	if err != nil {
		log.Panic(err)
	}

	if (stat.Mode() & os.ModePerm) != 0400 {
		log.Panicf("key file %v must have perms set to 0400", keyFilename)
	}

	content, err := os.ReadFile(keyFilename)

	if err != nil {
		log.Panic(err)
	}

	key, err := base64.StdEncoding.Strict().DecodeString(string(content))
	if err != nil {
		log.Panic(err)
	}

	return key
}

func shred(key *[]byte) {
	for i := range *key {
		(*key)[i] = 0x69
	}
}

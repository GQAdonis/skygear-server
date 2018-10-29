// Copyright 2015-present Oursky Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cloud

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/franela/goreq"
	"github.com/sirupsen/logrus"
	"github.com/skygeario/skygear-server/pkg/core/asset"
)

const (
	cloudAssetURLExpiryInterval          = 15 * time.Minute
	cloudAssetSignerTokenRefreshInterval = 30 * time.Minute
	cloudAssetSignerTokenExpiryInterval  = 2 * time.Hour
)

// AssetStore models the skygear cloud asset store
type AssetStore struct {
	appName   string
	host      string
	authToken string
	urlPrefix string
	public    bool
	signer    *cloudStoreSigner
	logger    *logrus.Entry
}

type refreshSignerTokenResponse struct {
	Value     string    `json:"value"`
	Extra     string    `json:"extra"`
	ExpiredAt time.Time `json:"expired_at"`
}

// NewAssetStore creates a new cloud asset store
func NewAssetStore(
	appName string,
	host string,
	authToken string,
	publicURLPrefix string,
	privateURLPrefix string,
	public bool,
	logger *logrus.Entry,
) (*AssetStore, error) {
	if appName == "" {
		return nil, errors.New("Missing app name for cloud asset")
	}

	if host == "" {
		return nil, errors.New("Missing host for cloud asset")
	}

	if authToken == "" {
		return nil, errors.New("Missing auth token for cloud asset")
	}

	if public && publicURLPrefix == "" {
		return nil, errors.New("Missing public URL prefix for cloud asset")
	}

	if !public && privateURLPrefix == "" {
		return nil, errors.New("Missing private URL prefix for cloud asset")
	}

	urlPrefix := privateURLPrefix
	if public {
		urlPrefix = publicURLPrefix
	}

	store := &AssetStore{
		appName:   appName,
		host:      host,
		authToken: authToken,
		public:    public,
		urlPrefix: urlPrefix,
		logger:    logger,
	}

	store.signer = newCloudStoreSigner(
		cloudAssetSignerTokenRefreshInterval,
		store.refreshSignerToken,
		logger,
	)
	go store.refreshSignerToken()

	logger.
		WithField("cloud-store", store).
		Info("Created Cloud Asset Store")

	return store, nil
}

func (s *AssetStore) refreshSignerToken() {
	s.logger.Info("Start refresh Cloud Asset Signer Token")

	urlString := strings.Join(
		[]string{
			s.host,
			"token",
			url.PathEscape(s.appName),
		},
		"/",
	)
	expiredAt := time.Now().
		Add(cloudAssetSignerTokenExpiryInterval).
		Unix()

	req := goreq.Request{
		Uri:     urlString,
		Timeout: 10 * time.Second,
		QueryString: struct {
			ExpiredAt int64 `url:"expired_at"`
		}{expiredAt},
	}.WithHeader("Authorization", "Bearer "+s.authToken)

	res, err := req.Do()
	if err != nil {
		s.logger.WithFields(logrus.Fields{
			"url":        urlString,
			"expired-at": expiredAt,
			"error":      err,
		}).Error("Fail to request to refresh Cloud Asset Signer Token")

		return
	}

	resBody := refreshSignerTokenResponse{}
	err = res.Body.FromJsonTo(&resBody)
	if err != nil {
		s.logger.WithFields(logrus.Fields{
			"error":    err,
			"response": res.Body,
		}).Error("Fail to parse the response for refresh Cloud Asset Signer Token")

		return
	}

	s.logger.
		WithField("response", resBody).
		Info("Successfully got new Cloud Asset Signer Token")

	s.signer.update(resBody.Value, resBody.Extra, resBody.ExpiredAt)
}

// GetFileReader returns a reader for reading files
func (s AssetStore) GetFileReader(name string) (io.ReadCloser, error) {
	return nil, errors.New(
		"Directly getting files is not available for cloud-based asset store",
	)
}

// PutFileReader return a writer for uploading files
func (s AssetStore) PutFileReader(
	name string,
	src io.Reader,
	length int64,
	contentType string,
) error {
	return errors.New(
		"Directly uploading files is not available for cloud-based asset store",
	)
}

// GeneratePostFileRequest return a PostFileRequest for uploading asset
func (s AssetStore) GeneratePostFileRequest(name string, contentType string, length int64) (*asset.PostFileRequest, error) {
	s.logger.
		WithField("name", name).
		Info("Start generate post file request for Cloud Asset")

	urlString := strings.Join(
		[]string{
			s.host,
			"asset",
			url.PathEscape(s.appName),
			url.PathEscape(name),
		},
		"/",
	)

	req := goreq.Request{
		Method:      http.MethodPut,
		Uri:         urlString,
		Timeout:     10 * time.Second,
		ContentType: "application/json",
		Body: struct {
			ContentType string `json:"content-type,omitempty"`
			Length      int64  `json:"content-size,omitempty"`
		}{
			ContentType: contentType,
			Length:      length,
		},
	}.WithHeader("Authorization", "Bearer "+s.authToken)

	res, err := req.Do()
	if err != nil {
		s.logger.WithFields(logrus.Fields{
			"url":   urlString,
			"error": err,
		}).Error("Fail to request for pre-signed POST request")

		return nil, errors.New("Fail to request for pre-signed POST request")
	}

	if res.StatusCode != http.StatusOK {
		body, _ := res.Body.ToString()
		s.logger.WithFields(logrus.Fields{
			"url":    urlString,
			"status": res.StatusCode,
			"body":   body,
		}).Error("Fail to request for pre-signed POST request")

		return nil, errors.New("Fail to request for pre-signed POST request")
	}

	postRequest := &asset.PostFileRequest{}
	err = res.Body.FromJsonTo(postRequest)
	if err != nil {
		s.logger.WithFields(logrus.Fields{
			"error":    err,
			"response": res.Body,
		}).Error("Fail to parse the response of pre-signed POST request")

		return nil, errors.New("Fail to parse the response of pre-signed POST request")
	}

	return postRequest, nil
}

// SignedURL return a signed URL with expiry date
func (s AssetStore) SignedURL(name string) (string, error) {
	targetURLString := strings.Join(
		[]string{
			s.urlPrefix,
			url.PathEscape(s.appName),
			url.PathEscape(name),
		},
		"/",
	)

	targetURL, err := url.Parse(targetURLString)
	if err != nil {
		s.logger.WithFields(logrus.Fields{
			"error":        err,
			"unsigned-url": targetURLString,
		}).Error("Fail to parse the unsigned URL")

		return "", errors.New("Fail to parse the unsigned URL")
	}

	if !s.IsSignatureRequired() {
		return targetURL.String(), nil
	}

	signerToken, signerExtra, _ := s.signer.get()

	if signerToken == "" || signerExtra == "" {
		s.logger.WithFields(logrus.Fields{
			"signer-token": signerToken,
			"signer-extra": signerExtra,
		}).Warn("Cloud Asset Signer Token is not yet ready")

		return "", errors.New("Cloud Asset Signer Token is not yet ready")
	}

	expiredAt := time.Now().Add(cloudAssetURLExpiryInterval)
	expiredAtString := strconv.FormatInt(expiredAt.Unix(), 10)

	hash := hmac.New(sha256.New, []byte(signerToken))
	hash.Write([]byte(s.appName))
	hash.Write([]byte(name))
	hash.Write([]byte(expiredAtString))
	hash.Write([]byte(signerExtra))

	signature := base64.StdEncoding.EncodeToString(hash.Sum(nil))
	signatureAndExtra := strings.Join(
		[]string{signature, signerExtra},
		".",
	)

	targetURL.RawQuery = url.Values{
		"expired_at": []string{expiredAtString},
		"signature":  []string{signatureAndExtra},
	}.Encode()

	return targetURL.String(), nil
}

// IsSignatureRequired indicates whether a signature is required
func (s AssetStore) IsSignatureRequired() bool {
	return !s.public
}

// ParseSignature tries to parse the asset signature
func (s AssetStore) ParseSignature(
	signed string,
	name string,
	expiredAt time.Time,
) (bool, error) {

	return false, errors.New(
		"Asset signature parsing for cloud-based asset store is not available",
	)
}
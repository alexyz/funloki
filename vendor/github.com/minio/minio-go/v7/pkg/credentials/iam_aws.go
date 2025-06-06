/*
 * MinIO Go Library for Amazon S3 Compatible Cloud Storage
 * Copyright 2017 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package credentials

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7/internal/json"
)

// DefaultExpiryWindow - Default expiry window.
// ExpiryWindow will allow the credentials to trigger refreshing
// prior to the credentials actually expiring. This is beneficial
// so race conditions with expiring credentials do not cause
// request to fail unexpectedly due to ExpiredTokenException exceptions.
// DefaultExpiryWindow can be used as parameter to (*Expiry).SetExpiration.
// When used the tokens refresh will be triggered when 80% of the elapsed
// time until the actual expiration time is passed.
const DefaultExpiryWindow = -1

// A IAM retrieves credentials from the EC2 service, and keeps track if
// those credentials are expired.
type IAM struct {
	Expiry

	// Optional http Client to use when connecting to IAM metadata service
	// (overrides default client in CredContext)
	Client *http.Client

	// Custom endpoint to fetch IAM role credentials.
	Endpoint string

	// Region configurable custom region for STS
	Region string

	// Support for container authorization token https://docs.aws.amazon.com/sdkref/latest/guide/feature-container-credentials.html
	Container struct {
		AuthorizationToken     string
		AuthorizationTokenFile string
		CredentialsFullURI     string
		CredentialsRelativeURI string
	}

	// EKS based k8s RBAC authorization - https://docs.aws.amazon.com/eks/latest/userguide/pod-configuration.html
	EKSIdentity struct {
		TokenFile       string
		RoleARN         string
		RoleSessionName string
	}
}

// IAM Roles for Amazon EC2
// http://docs.aws.amazon.com/AWSEC2/latest/UserGuide/iam-roles-for-amazon-ec2.html
const (
	DefaultIAMRoleEndpoint      = "http://169.254.169.254"
	DefaultECSRoleEndpoint      = "http://169.254.170.2"
	DefaultSTSRoleEndpoint      = "https://sts.amazonaws.com"
	DefaultIAMSecurityCredsPath = "/latest/meta-data/iam/security-credentials/"
	TokenRequestTTLHeader       = "X-aws-ec2-metadata-token-ttl-seconds"
	TokenPath                   = "/latest/api/token"
	TokenTTL                    = "21600"
	TokenRequestHeader          = "X-aws-ec2-metadata-token"
)

// NewIAM returns a pointer to a new Credentials object wrapping the IAM.
func NewIAM(endpoint string) *Credentials {
	return New(&IAM{
		Endpoint: endpoint,
	})
}

// RetrieveWithCredContext is like Retrieve with Cred Context
func (m *IAM) RetrieveWithCredContext(cc *CredContext) (Value, error) {
	if cc == nil {
		cc = defaultCredContext
	}

	token := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN")
	if token == "" {
		token = m.Container.AuthorizationToken
	}

	tokenFile := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE")
	if tokenFile == "" {
		tokenFile = m.Container.AuthorizationToken
	}

	relativeURI := os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")
	if relativeURI == "" {
		relativeURI = m.Container.CredentialsRelativeURI
	}

	fullURI := os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI")
	if fullURI == "" {
		fullURI = m.Container.CredentialsFullURI
	}

	identityFile := os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE")
	if identityFile == "" {
		identityFile = m.EKSIdentity.TokenFile
	}

	roleArn := os.Getenv("AWS_ROLE_ARN")
	if roleArn == "" {
		roleArn = m.EKSIdentity.RoleARN
	}

	roleSessionName := os.Getenv("AWS_ROLE_SESSION_NAME")
	if roleSessionName == "" {
		roleSessionName = m.EKSIdentity.RoleSessionName
	}

	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = m.Region
	}

	var roleCreds ec2RoleCredRespBody
	var err error

	client := m.Client
	if client == nil {
		client = cc.Client
	}
	if client == nil {
		client = defaultCredContext.Client
	}

	endpoint := m.Endpoint

	switch {
	case identityFile != "":
		if len(endpoint) == 0 {
			if region != "" {
				if strings.HasPrefix(region, "cn-") {
					endpoint = "https://sts." + region + ".amazonaws.com.cn"
				} else {
					endpoint = "https://sts." + region + ".amazonaws.com"
				}
			} else {
				endpoint = DefaultSTSRoleEndpoint
			}
		}

		creds := &STSWebIdentity{
			Client:      client,
			STSEndpoint: endpoint,
			GetWebIDTokenExpiry: func() (*WebIdentityToken, error) {
				token, err := os.ReadFile(identityFile)
				if err != nil {
					return nil, err
				}

				return &WebIdentityToken{Token: string(token)}, nil
			},
			RoleARN:         roleArn,
			roleSessionName: roleSessionName,
		}

		stsWebIdentityCreds, err := creds.RetrieveWithCredContext(cc)
		if err == nil {
			m.SetExpiration(creds.Expiration(), DefaultExpiryWindow)
		}
		return stsWebIdentityCreds, err

	case relativeURI != "":
		if len(endpoint) == 0 {
			endpoint = fmt.Sprintf("%s%s", DefaultECSRoleEndpoint, relativeURI)
		}

		roleCreds, err = getEcsTaskCredentials(client, endpoint, token)

	case tokenFile != "" && fullURI != "":
		endpoint = fullURI
		roleCreds, err = getEKSPodIdentityCredentials(client, endpoint, tokenFile)

	case fullURI != "":
		if len(endpoint) == 0 {
			endpoint = fullURI
			var ok bool
			if ok, err = isLoopback(endpoint); !ok {
				if err == nil {
					err = fmt.Errorf("uri host is not a loopback address: %s", endpoint)
				}
				break
			}
		}

		roleCreds, err = getEcsTaskCredentials(client, endpoint, token)

	default:
		roleCreds, err = getCredentials(client, endpoint)
	}

	if err != nil {
		return Value{}, err
	}
	// Expiry window is set to 10secs.
	m.SetExpiration(roleCreds.Expiration, DefaultExpiryWindow)

	return Value{
		AccessKeyID:     roleCreds.AccessKeyID,
		SecretAccessKey: roleCreds.SecretAccessKey,
		SessionToken:    roleCreds.Token,
		Expiration:      roleCreds.Expiration,
		SignerType:      SignatureV4,
	}, nil
}

// Retrieve retrieves credentials from the EC2 service.
// Error will be returned if the request fails, or unable to extract
// the desired
func (m *IAM) Retrieve() (Value, error) {
	return m.RetrieveWithCredContext(nil)
}

// A ec2RoleCredRespBody provides the shape for unmarshaling credential
// request responses.
type ec2RoleCredRespBody struct {
	// Success State
	Expiration      time.Time
	AccessKeyID     string
	SecretAccessKey string
	Token           string

	// Error state
	Code    string
	Message string

	// Unused params.
	LastUpdated time.Time
	Type        string
}

// Get the final IAM role URL where the request will
// be sent to fetch the rolling access credentials.
// http://docs.aws.amazon.com/AWSEC2/latest/UserGuide/iam-roles-for-amazon-ec2.html
func getIAMRoleURL(endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	u.Path = DefaultIAMSecurityCredsPath
	return u, nil
}

// listRoleNames lists of credential role names associated
// with the current EC2 service. If there are no credentials,
// or there is an error making or receiving the request.
// http://docs.aws.amazon.com/AWSEC2/latest/UserGuide/iam-roles-for-amazon-ec2.html
func listRoleNames(client *http.Client, u *url.URL, token string) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Add(TokenRequestHeader, token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}

	credsList := []string{}
	s := bufio.NewScanner(resp.Body)
	for s.Scan() {
		credsList = append(credsList, s.Text())
	}

	if err := s.Err(); err != nil {
		return nil, err
	}

	return credsList, nil
}

func getEcsTaskCredentials(client *http.Client, endpoint, token string) (ec2RoleCredRespBody, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return ec2RoleCredRespBody{}, err
	}

	if token != "" {
		req.Header.Set("Authorization", token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return ec2RoleCredRespBody{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ec2RoleCredRespBody{}, errors.New(resp.Status)
	}

	respCreds := ec2RoleCredRespBody{}
	if err := json.NewDecoder(resp.Body).Decode(&respCreds); err != nil {
		return ec2RoleCredRespBody{}, err
	}

	return respCreds, nil
}

func getEKSPodIdentityCredentials(client *http.Client, endpoint string, tokenFile string) (ec2RoleCredRespBody, error) {
	if tokenFile != "" {
		bytes, err := os.ReadFile(tokenFile)
		if err != nil {
			return ec2RoleCredRespBody{}, fmt.Errorf("getEKSPodIdentityCredentials: failed to read token file:%s", err)
		}
		token := string(bytes)
		return getEcsTaskCredentials(client, endpoint, token)
	}
	return ec2RoleCredRespBody{}, fmt.Errorf("getEKSPodIdentityCredentials: no tokenFile found")
}

func fetchIMDSToken(client *http.Client, endpoint string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint+TokenPath, nil)
	if err != nil {
		return "", err
	}
	req.Header.Add(TokenRequestTTLHeader, TokenTTL)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}
	return string(data), nil
}

// getCredentials - obtains the credentials from the IAM role name associated with
// the current EC2 service.
//
// If the credentials cannot be found, or there is an error
// reading the response an error will be returned.
func getCredentials(client *http.Client, endpoint string) (ec2RoleCredRespBody, error) {
	if endpoint == "" {
		endpoint = DefaultIAMRoleEndpoint
	}

	// https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-instance-metadata-service.html
	token, err := fetchIMDSToken(client, endpoint)
	if err != nil {
		// Return only errors for valid situations, if the IMDSv2 is not enabled
		// we will not be able to get the token, in such a situation we have
		// to rely on IMDSv1 behavior as a fallback, this check ensures that.
		// Refer https://github.com/minio/minio-go/issues/1866
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			return ec2RoleCredRespBody{}, err
		}
	}

	// http://docs.aws.amazon.com/AWSEC2/latest/UserGuide/iam-roles-for-amazon-ec2.html
	u, err := getIAMRoleURL(endpoint)
	if err != nil {
		return ec2RoleCredRespBody{}, err
	}

	// http://docs.aws.amazon.com/AWSEC2/latest/UserGuide/iam-roles-for-amazon-ec2.html
	roleNames, err := listRoleNames(client, u, token)
	if err != nil {
		return ec2RoleCredRespBody{}, err
	}

	if len(roleNames) == 0 {
		return ec2RoleCredRespBody{}, errors.New("No IAM roles attached to this EC2 service")
	}

	// http://docs.aws.amazon.com/AWSEC2/latest/UserGuide/iam-roles-for-amazon-ec2.html
	// - An instance profile can contain only one IAM role. This limit cannot be increased.
	roleName := roleNames[0]

	// http://docs.aws.amazon.com/AWSEC2/latest/UserGuide/iam-roles-for-amazon-ec2.html
	// The following command retrieves the security credentials for an
	// IAM role named `s3access`.
	//
	//    $ curl http://169.254.169.254/latest/meta-data/iam/security-credentials/s3access
	//
	u.Path = path.Join(u.Path, roleName)
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return ec2RoleCredRespBody{}, err
	}
	if token != "" {
		req.Header.Add(TokenRequestHeader, token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return ec2RoleCredRespBody{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ec2RoleCredRespBody{}, errors.New(resp.Status)
	}

	respCreds := ec2RoleCredRespBody{}
	if err := json.NewDecoder(resp.Body).Decode(&respCreds); err != nil {
		return ec2RoleCredRespBody{}, err
	}

	if respCreds.Code != "Success" {
		// If an error code was returned something failed requesting the role.
		return ec2RoleCredRespBody{}, errors.New(respCreds.Message)
	}

	return respCreds, nil
}

// isLoopback identifies if a uri's host is on a loopback address
func isLoopback(uri string) (bool, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return false, err
	}

	host := u.Hostname()
	if len(host) == 0 {
		return false, fmt.Errorf("can't parse host from uri: %s", uri)
	}

	ips, err := net.LookupHost(host)
	if err != nil {
		return false, err
	}
	for _, ip := range ips {
		if !net.ParseIP(ip).IsLoopback() {
			return false, nil
		}
	}

	return true, nil
}

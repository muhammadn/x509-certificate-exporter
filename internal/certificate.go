package internal

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	v1 "k8s.io/api/core/v1"
)

// YAMLCertRef : Contains information to access certificates in yaml files
type YAMLCertRef struct {
	CertMatchExpr string
	IDMatchExpr   string
	Format        YAMLCertFormat
}

// YAMLCertFormat : Type of cert encoding in YAML files
type YAMLCertFormat int

// YAMLCertFormat : Impl
const (
	YAMLCertFormatFile   YAMLCertFormat = iota
	YAMLCertFormatBase64                = iota
)

// DefaultYamlPaths : Pre-written paths for some k8s config files
var DefaultYamlPaths = []YAMLCertRef{
	{
		CertMatchExpr: "clusters.[*].cluster.certificate-authority-data",
		IDMatchExpr:   "clusters.[*].name",
		Format:        YAMLCertFormatBase64,
	},
	{
		CertMatchExpr: "clusters.[*].cluster.certificate-authority",
		IDMatchExpr:   "clusters.[*].name",
		Format:        YAMLCertFormatFile,
	},
	{
		CertMatchExpr: "users.[*].user.client-certificate-data",
		IDMatchExpr:   "users.[*].name",
		Format:        YAMLCertFormatBase64,
	},
	{
		CertMatchExpr: "users.[*].user.client-certificate",
		IDMatchExpr:   "users.[*].name",
		Format:        YAMLCertFormatFile,
	},
}

type certificateRef struct {
	path         string
	format       certificateFormat
	certificates []*parsedCertificate
	userIDs      []string

	yamlPaths  []YAMLCertRef
	kubeSecret *v1.Secret
}

type parsedCertificate struct {
	cert        *x509.Certificate
	userID      string
	yqMatchExpr string
}

type certificateError struct {
	err error
	ref *certificateRef
}

type certificateFormat int

const (
	certificateFormatPEM        certificateFormat = iota
	certificateFormatYAML                         = iota
	certificateFormatKubeSecret                   = iota
)

func (cert *certificateRef) parse() error {
	var err error

	switch cert.format {
	case certificateFormatPEM:
		cert.certificates, err = readAndParsePEMFile(cert.path)
	case certificateFormatYAML:
		cert.certificates, err = readAndParseYAMLFile(cert.path, cert.yamlPaths)
	case certificateFormatKubeSecret:
		cert.certificates, err = readAndParseKubeSecret(cert.path, cert.kubeSecret)
	}

	return err
}

func readAndParsePEMFile(path string) ([]*parsedCertificate, error) {
	contents, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	output := []*parsedCertificate{}
	certs, err := parsePEM(contents)
	if err != nil {
		return nil, err
	}

	for _, cert := range certs {
		output = append(output, &parsedCertificate{cert: cert})
	}

	return output, nil
}

func readAndParseYAMLFile(filePath string, yamlPaths []YAMLCertRef) ([]*parsedCertificate, error) {
	output := []*parsedCertificate{}

	for _, exprs := range yamlPaths {
		rawCerts, err := exec.Command("yq", "r", filePath, exprs.CertMatchExpr).CombinedOutput()
		if err != nil {
			return nil, errors.New(err.Error() + " | stderr: " + string(rawCerts))
		}
		if len(rawCerts) == 0 {
			continue
		}

		var decodedCerts []byte
		if exprs.Format == YAMLCertFormatBase64 {
			decodedCerts = make([]byte, base64.StdEncoding.DecodedLen(len(rawCerts)))
			base64.StdEncoding.Decode(decodedCerts, []byte(rawCerts))
		} else if exprs.Format == YAMLCertFormatFile {
			certPath := path.Join(filepath.Dir(filePath), string(rawCerts))
			decodedCerts, err = ioutil.ReadFile(strings.TrimRight(certPath, "\n"))
			if err != nil {
				return nil, err
			}
		}

		certs, err := parsePEM(decodedCerts)
		if err != nil {
			return nil, err
		}

		rawUserIDs, _ := exec.Command("yq", "r", filePath, exprs.IDMatchExpr).Output()
		userIDs := []string{}
		for _, userID := range strings.Split(string(rawUserIDs), "\n") {
			if userID != "" {
				userIDs = append(userIDs, userID)
			}
		}
		if len(userIDs) != len(certs) {
			return nil, fmt.Errorf("failed to parse some labels in %s (got %d IDs but %d certs for \"%s\")", filePath, len(userIDs), len(certs), exprs.IDMatchExpr)
		}

		for index, cert := range certs {
			output = append(output, &parsedCertificate{
				cert:        cert,
				userID:      userIDs[index],
				yqMatchExpr: exprs.CertMatchExpr,
			})
		}
	}

	return output, nil
}

func readAndParseKubeSecret(path string, secret *v1.Secret) ([]*parsedCertificate, error) {
	key := "tls.crt"
	if _, ok := secret.Data[key]; !ok {
		return nil, fmt.Errorf("secret \"%s\" has no key \"%s\"", secret.GetName(), key)
	}

	certs, err := parsePEM(secret.Data[key])
	if err != nil {
		return nil, err
	}

	output := []*parsedCertificate{}
	for _, cert := range certs {
		output = append(output, &parsedCertificate{
			cert: cert,
		})
	}

	return output, nil
}

func parsePEM(data []byte) ([]*x509.Certificate, error) {
	output := []*x509.Certificate{}

	for {
		block, rest := pem.Decode(data)
		if block == nil {
			break
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("tried to parse malformed x509 data, %s", err.Error())
		}

		output = append(output, cert)
		data = rest
	}

	return output, nil
}

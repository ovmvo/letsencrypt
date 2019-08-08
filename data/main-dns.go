package data

// An example of the acme library to create a simple certbot-like clone. Takes a few command line parameters and issues
// a certificate using the http-01 challenge method.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io/ioutil"
	"letsencrypt/acme"
	"log"
	"math/big"
	"os"
	"strings"
)

var (
	domains      string
	directoryUrl string
	contactsList string
	accountFile  string
	certFile     string
	keyFile      string
)

type acmeAccountFile struct {
	Url        string            `json:"url"`
	PrivateKey *ecdsa.PrivateKey `json:"privateKey"`
}

type tmpCurve struct {
	P, N, B, Gx, Gy *big.Int
	BitSize         int
	Name            string
}

type tmpPrivateKey struct {
	D, X, Y *big.Int
	Curve   *tmpCurve
}

type tmpAccountFile struct {
	Url        string         `json:"url"`
	PrivateKey tmpPrivateKey `json:"privateKey"`
}

func main() {
	flag.StringVar(&directoryUrl, "dirurl", acme.LetsEncryptStaging,
		"acme directory url - defaults to lets encrypt v2 staging url if not provided")
	flag.StringVar(&contactsList, "contact", "",
		"a list of comma separated contact emails to use when creating a new account (optional, dont include 'mailto:' prefix)")
	flag.StringVar(&domains, "domains", "nvtia.com,*.nvtia.com",
		"a comma separated list of domains to issue a certificate for")
	flag.StringVar(&accountFile, "accountfile", "data/cache/account.json",
		"the file that the account json data will be saved to/loaded from (will create new file if not exists)")
	flag.StringVar(&certFile, "certfile", "data/ssl/cert.pem",
		"the file that the pem encoded certificate chain will be saved to")
	flag.StringVar(&keyFile, "keyfile", "data/ssl/key.pem",
		"the file that the pem encoded certificate private key will be saved to")
	flag.Parse()

	// check domains are provided
	if domains == "" {
		log.Fatal("No domains provided")
	}

	// create a new acme client given a provided (or default) directory url
	log.Printf("Connecting to acme directory url: %s", directoryUrl)
	client, err := acme.NewClient(directoryUrl)
	if err != nil {
		log.Fatalf("Error connecting to acme directory: %v", err)
	}

	// attempt to load an existing account from file
	log.Printf("Loading account file %s", accountFile)
	account, err := loadAccount(client)
	if err != nil {
		log.Printf("Error loading existing account: %v", err)
		// if there was an error loading an account, just create a new one
		log.Printf("Creating new account")
		account, err = createAccount(client)
		if err != nil {
			log.Fatalf("Error creaing new account: %v", err)
		}
	}
	log.Printf("Account url: %s", account.URL)

	// collect the comma separated domains into acme identifiers
	domainList := strings.Split(domains, ",")

	var ids []acme.Identifier
	for _, domain := range domainList {
		ids = append(ids, acme.Identifier{Type: "dns", Value: domain})
	}

	// create a new order with the acme service given the provided identifiers
	log.Printf("Creating new order for domains: %s", domainList)
	order, err := client.NewOrder(account, ids)
	if err != nil {
		log.Fatalf("Error creating new order: %v", err)
	}
	log.Printf("Order created: %s", order.URL)

	// loop through each of the provided authorization urls
	url := make(chan string)
	for _, authUrl := range order.Authorizations {
		// fetch the authorization data from the acme service given the provided authorization url
		go func(authUrl string) {
			log.Printf("Fetching authorization: %s", authUrl)
			auth, err := client.FetchAuthorization(account, authUrl)
			if err != nil {
				log.Fatalf("Error fetching authorization url %q: %v", authUrl, err)
			}
			log.Printf("Fetched authorization: %s", auth.Identifier.Value)

			// grab a http-01 challenge from the authorization if it exists
			chal, ok := auth.ChallengeMap[acme.ChallengeTypeDNS01]
			if !ok {
				log.Fatalf("Unable to find dns challenge for auth %s", auth.Identifier.Value)
			}

			fmt.Printf("_acme-challenge.%s : %s\n", auth.Identifier.Value, acme.EncodeDNS01KeyAuthorization(chal.KeyAuthorization))

			resut := bool(false)
			resList := acme.NewTxtChange("_acme-challenge." + auth.Identifier.Value)
				for _, res := range resList {
					if res == acme.EncodeDNS01KeyAuthorization(chal.KeyAuthorization) {
						resut = true
						break
					}
				}
			if !resut {
				log.Fatal("解析错误")
			}

			log.Printf("Updating challenge for authorization %s: %s", auth.Identifier.Value, chal.URL)
			// update the acme server that the challenge file is ready to be queried
			chal, err = client.UpdateChallenge(account, chal)
			if err != nil {
				log.Fatalf("Error updating authorization %s challenge: %v", auth.Identifier.Value, err)
				//log.Fatalf("Error updating authorization challenge: %v", err)
			}
			url <- auth.Identifier.Value
		}(authUrl)
	}
	// all the challenges should now be completed
	for i := 1; i <= len(domainList); i++ {
		log.Printf("%s Challenge updated", <-url)
	}

	// create a csr for the new certificate
	log.Printf("Generating certificate private key")
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("Error generating certificate key: %v", err)
	}
	// encode the new ec private key
	certKeyEnc, err := x509.MarshalECPrivateKey(certKey)
	if err != nil {
		log.Fatalf("Error encoding certificate key file: %v", err)
	}

	// write the key to the key file as a pem encoded key
	log.Printf("Writing key file: %s", keyFile)
	if err := ioutil.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: certKeyEnc,
	}), 0600); err != nil {
		log.Fatalf("Error writing key file %q: %v", keyFile, err)
	}

	// create the new csr template
	log.Printf("Creating csr")
	tpl := &x509.CertificateRequest{
		SignatureAlgorithm: x509.ECDSAWithSHA256,
		PublicKeyAlgorithm: x509.ECDSA,
		PublicKey:          certKey.Public(),
		Subject:            pkix.Name{CommonName: domainList[0]},
		DNSNames:           domainList,
	}
	csrDer, err := x509.CreateCertificateRequest(rand.Reader, tpl, certKey)
	if err != nil {
		log.Fatalf("Error creating certificate request: %v", err)
	}
	csr, err := x509.ParseCertificateRequest(csrDer)
	if err != nil {
		log.Fatalf("Error parsing certificate request: %v", err)
	}

	// finalize the order with the acme server given a csr
	log.Printf("Finalising order: %s", order.URL)
	order, err = client.FinalizeOrder(account, order, csr)
	if err != nil {
		log.Fatalf("Error finalizing order: %v", err)
	}

	// fetch the certificate chain from the finalized order provided by the acme server
	log.Printf("Fetching certificate: %s", order.Certificate)
	certs, err := client.FetchCertificates(account, order.Certificate)
	if err != nil {
		log.Fatalf("Error fetching order certificates: %v", err)
	}

	// write the pem encoded certificate chain to file
	log.Printf("Saving certificate to: %s", certFile)
	var pemData []string
	for _, c := range certs {
		pemData = append(pemData, strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: c.Raw,
		}))))
	}
	if err := ioutil.WriteFile(certFile, []byte(strings.Join(pemData, "\n")), 0600); err != nil {
		log.Fatalf("Error writing certificate file %q: %v", certFile, err)
	}

	log.Printf("Done.")
}


func loadAccount(client acme.Client) (acme.Account, error) {
	if _, err := os.Stat(accountFile); err != nil {
		return acme.Account{}, err
	}
	raw, err := ioutil.ReadFile(accountFile)
	if err != nil {
		return acme.Account{}, err
	}
	var pp bytes.Buffer
	json.Indent(&pp, raw, " ", "  ")

	var taf tmpAccountFile
	if err := json.Unmarshal(raw, &taf); err != nil {
		return acme.Account{}, fmt.Errorf("error reading account file: %v", err)
	}

	var apkey ecdsa.PrivateKey
	apkey.D = taf.PrivateKey.D
	apkey.X = taf.PrivateKey.X
	apkey.Y = taf.PrivateKey.Y
	apkey.Curve = elliptic.P256()

	//b, err := x509.MarshalECPrivateKey(&apkey)
	//
	//if err != nil {
	//	log.Println("wwwwwww")
	//}
	//fmt.Println(string(pem.EncodeToMemory(&pem.Block{Type:"EC PRIVATE KEY", Bytes:b})))

	acct := acme.Account{PrivateKey: &apkey, URL: taf.Url}

	account, err := client.UpdateAccount(acct, true, getContacts()...)
	if err != nil {
		return acme.Account{}, fmt.Errorf("error updating existing account: %v", err)
	}
	return account, nil
}

func createAccount(client acme.Client) (acme.Account, error) {
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return acme.Account{}, fmt.Errorf("error creating private key: %v", err)
	}
	account, err := client.NewAccount(privKey, false, true, getContacts()...)
	if err != nil {
		return acme.Account{}, fmt.Errorf("error creating new account: %v", err)
	}
	raw, err := json.Marshal(acmeAccountFile{PrivateKey: privKey, Url: account.URL})
	if err != nil {
		return acme.Account{}, fmt.Errorf("error parsing new account: %v", err)
	}
	if err := ioutil.WriteFile(accountFile, raw, 0600); err != nil {
		return acme.Account{}, fmt.Errorf("error creating account file: %v", err)
	}
	return account, nil
}

func getContacts() []string {
	var contacts []string
	if contactsList != "" {
		contacts = strings.Split(contactsList, ",")
		for i := 0; i < len(contacts); i++ {
			contacts[i] = "mailto:" + contacts[i]
		}
	}
	return contacts
}
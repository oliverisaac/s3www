package main

import (
	"context"
	"flag"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"
	"github.com/patrickmn/go-cache"
)

// S3 - A S3 implements FileSystem using the minio client
// allowing access to your S3 buckets and objects.
//
// Note that S3 will allow all access to files in your private
// buckets, If you have any sensitive information please make
// sure to not sure this project.
type S3 struct {
	*minio.Client
	bucket string
	cache  *cache.Cache
}

func pathIsDir(ctx context.Context, s3 *S3, name string) bool {
	name = strings.Trim(name, pathSeparator) + pathSeparator
	if name == pathSeparator {
		return true
	}

	if boolIface, ok := s3.cache.Get(name); ok {
		return boolIface.(bool)
	}

	var ret bool
	listCtx, cancel := context.WithCancel(ctx)

	objCh := s3.Client.ListObjects(listCtx,
		s3.bucket,
		minio.ListObjectsOptions{
			Prefix: name,
		})
	for _ = range objCh {
		cancel()
		ret = true
	}
	s3.cache.SetDefault(name, ret)
	return ret
}

// Open - implements http.Filesystem implementation.
func (s3 *S3) Open(name string) (http.File, error) {
	if pathIsDir(context.Background(), s3, name) {
		return &httpMinioObject{
			client: s3.Client,
			object: nil,
			isDir:  true,
			bucket: bucket,
			prefix: strings.TrimSuffix(name, pathSeparator),
		}, nil
	}

	name = strings.TrimPrefix(name, pathSeparator)
	obj, err := getObject(context.Background(), s3, name)
	if err != nil {
		return nil, os.ErrNotExist
	}

	return &httpMinioObject{
		client: s3.Client,
		object: obj,
		isDir:  false,
		bucket: bucket,
		prefix: name,
	}, nil
}

func getObject(ctx context.Context, s3 *S3, name string) (*minio.Object, error) {
	names := [4]string{name, name + "/index.html", name + "/index.htm", "/404.html"}
	for _, n := range names {
		obj, err := s3.Client.GetObject(ctx, s3.bucket, n, minio.GetObjectOptions{})
		if err != nil {
			log.Println(err)
			continue
		}

		_, err = obj.Stat()
		if err != nil {
			// do not log "file" in bucket not found errors
			if minio.ToErrorResponse(err).Code != "NoSuchKey" {
				log.Println(err)
			}
			continue
		}

		return obj, nil
	}

	return nil, os.ErrNotExist
}

var (
	endpoint      string
	accessKey     string
	accessKeyFile string
	secretKey     string
	secretKeyFile string
	address       string
	bucket        string
	tlsCert       string
	tlsKey        string
	cacheTime     string
	letsEncrypt   bool
)

func init() {
	flag.StringVar(&endpoint, "endpoint", defaultEnvString("S3WWW_ENDPOINT", ""), "S3 server endpoint")
	flag.StringVar(&accessKey, "accessKey", defaultEnvString("S3WWW_ACCESS_KEY", ""), "Access key of S3 storage")
	flag.StringVar(&accessKeyFile, "accessKeyFile", defaultEnvString("S3WWW_ACCESS_KEY_FILE", ""), "File which contains the access key")
	flag.StringVar(&secretKey, "secretKey", defaultEnvString("S3WWW_SECRET_KEY", ""), "Secret key of S3 storage")
	flag.StringVar(&secretKeyFile, "secretKeyFile", defaultEnvString("S3WWW_SECRET_KEY_FILE", ""), "File which contains the Secret key")
	flag.StringVar(&bucket, "bucket", defaultEnvString("S3WWW_BUCKET", ""), "Bucket name which hosts static files")
	flag.StringVar(&address, "address", defaultEnvString("S3WWW_ADDRESS", "127.0.0.1:8080"), "Bind to a specific ADDRESS:PORT, ADDRESS can be an IP or hostname")
	flag.StringVar(&tlsCert, "ssl-cert", defaultEnvString("S3WWW_SSL_CERT", ""), "TLS certificate for this server")
	flag.StringVar(&tlsKey, "ssl-key", defaultEnvString("S3WWW_SSL_KEY", ""), "TLS private key for this server")
	flag.StringVar(&cacheTime, "cache-time", defaultEnvString("S3WWW_CACHE_TIME", "5m"), "Time to keep cache about directory listings")
	flag.BoolVar(&letsEncrypt, "lets-encrypt", defaultEnvBool("S3WWW_LETS_ENCRYPT", false), "Enable Let's Encrypt")
}

func defaultEnvString(key string, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

func defaultEnvBool(key string, defaultVal bool) bool {
	if val, ok := os.LookupEnv(key); ok {
		parsedVal, err := strconv.ParseBool(val)
		if err == nil {
			return parsedVal
		}
		log.Printf("String of %q did not parse as bool for env var %q", val, key)
	}
	return defaultVal
}

// NewCustomHTTPTransport returns a new http configuration
// used while communicating with the cloud backends.
// This sets the value for MaxIdleConnsPerHost from 2 (go default)
// to 100.
func NewCustomHTTPTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   1024,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
}

func main() {
	flag.Parse()

	if strings.TrimSpace(bucket) == "" {
		log.Fatalln(`Bucket name cannot be empty, please provide 's3www -bucket "mybucket"'`)
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		log.Fatalln(err)
	}

	// Chains all credential types, in the following order:
	//  - AWS env vars (i.e. AWS_ACCESS_KEY_ID)
	//  - AWS creds file (i.e. AWS_SHARED_CREDENTIALS_FILE or ~/.aws/credentials)
	//  - IAM profile based credentials. (performs an HTTP
	//    call to a pre-defined endpoint, only valid inside
	//    configured ec2 instances)
	var defaultAWSCredProviders = []credentials.Provider{
		&credentials.EnvAWS{},
		&credentials.FileAWSCredentials{},
		&credentials.IAM{
			Client: &http.Client{
				Transport: NewCustomHTTPTransport(),
			},
		},
		&credentials.EnvMinio{},
	}
	if accessKeyFile != "" {
		if keyBytes, err := ioutil.ReadFile(accessKeyFile); err == nil {
			accessKey = strings.TrimSpace(string(keyBytes))
		} else {
			log.Fatalf("Failed to read access key file %q", accessKeyFile)
		}
	}
	if secretKeyFile != "" {
		if keyBytes, err := ioutil.ReadFile(secretKeyFile); err == nil {
			secretKey = strings.TrimSpace(string(keyBytes))
		} else {
			log.Fatalf("Failed to read secret key file %q", secretKeyFile)
		}
	}
	if accessKey != "" && secretKey != "" {
		defaultAWSCredProviders = []credentials.Provider{
			&credentials.Static{
				Value: credentials.Value{
					AccessKeyID:     accessKey,
					SecretAccessKey: secretKey,
				},
			},
		}
	}

	// If we see an Amazon S3 endpoint, then we use more ways to fetch backend credentials.
	// Specifically IAM style rotating credentials are only supported with AWS S3 endpoint.
	creds := credentials.NewChainCredentials(defaultAWSCredProviders)

	client, err := minio.New(u.Host, &minio.Options{
		Creds:        creds,
		Secure:       u.Scheme == "https",
		Region:       s3utils.GetRegionFromURL(*u),
		BucketLookup: minio.BucketLookupAuto,
		Transport:    NewCustomHTTPTransport(),
	})
	if err != nil {
		log.Fatalln(err)
	}

	cacheDuration, err := time.ParseDuration(cacheTime)
	if err != nil {
		log.Fatalln(err)
	}

	s3 := &S3{
		Client: client,
		bucket: bucket,
		cache:  cache.New(cacheDuration, 10*time.Minute),
	}

	mux := http.FileServer(s3)
	if letsEncrypt {
		log.Printf("Started listening on https://%s\n", address)
		certmagic.HTTPS([]string{address}, mux)
	} else if tlsCert != "" && tlsKey != "" {
		log.Printf("Started listening on https://%s\n", address)
		log.Fatalln(http.ListenAndServeTLS(address, tlsCert, tlsKey, mux))
	} else {
		log.Printf("Started listening on http://%s\n", address)
		log.Fatalln(http.ListenAndServe(address, mux))
	}
}

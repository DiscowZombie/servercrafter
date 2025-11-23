package resolve

import (
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const s3SigningService = "s3"

// s3EmptyPayloadHash is the SHA-256 of an empty body, the payload of every download request
const s3EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// s3Auth signs requests with AWS Signature V4, for any S3 compatible store (AWS, Scaleway, MinIO…)
type s3Auth struct {
	signer      *v4.Signer
	credentials aws.Credentials
	region      string
}

func newS3Auth(accessKey string, secretKey string, region string) *s3Auth {
	return &s3Auth{
		signer: v4.NewSigner(func(o *v4.SignerOptions) {
			// S3 signs the path as sent, without the second escaping pass other AWS services expect
			o.DisableURIPathEscaping = true
		}),
		credentials: aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey},
		region:      region,
	}
}

func (s *s3Auth) Apply(r *http.Request) error {
	r.Header.Set("X-Amz-Content-Sha256", s3EmptyPayloadHash)
	return s.signer.SignHTTP(r.Context(), s.credentials, r, s3EmptyPayloadHash, s3SigningService, s.region, time.Now())
}

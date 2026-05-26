package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/aahl/tgnas/store"
)

var (
	ErrInvalidAccessKeyID      = errors.New("invalid access key id")
	ErrSignatureDoesNotMatch   = errors.New("signature does not match")
	ErrInvalidArgumentValue    = errors.New("invalid argument")
	ErrServiceUnavailableValue = errors.New("service unavailable")
)

type S3Error struct {
	Code    string
	Message string
	Status  int
}

type ErrorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId,omitempty"`
}

var (
	ErrNotImplemented       = S3Error{Code: "NotImplemented", Message: "A header you provided implies functionality that is not implemented.", Status: http.StatusNotImplemented}
	ErrEntityTooLarge       = S3Error{Code: "EntityTooLarge", Message: "Your proposed upload exceeds the maximum allowed object size.", Status: http.StatusBadRequest}
	ErrInvalidAccessKey     = S3Error{Code: "InvalidAccessKeyId", Message: "The AWS Access Key Id you provided does not exist in our records.", Status: http.StatusForbidden}
	ErrSignatureMismatch    = S3Error{Code: "SignatureDoesNotMatch", Message: "The request signature we calculated does not match the signature you provided.", Status: http.StatusForbidden}
	ErrNoSuchBucket         = S3Error{Code: "NoSuchBucket", Message: "The specified bucket does not exist.", Status: http.StatusNotFound}
	ErrNoSuchKey            = S3Error{Code: "NoSuchKey", Message: "The specified key does not exist.", Status: http.StatusNotFound}
	ErrInvalidRange         = S3Error{Code: "InvalidRange", Message: "The requested range is not satisfiable.", Status: http.StatusRequestedRangeNotSatisfiable}
	ErrMissingContentLength = S3Error{Code: "MissingContentLength", Message: "You must provide the Content-Length HTTP header.", Status: http.StatusLengthRequired}
	ErrNoSuchUpload         = S3Error{Code: "NoSuchUpload", Message: "The specified multipart upload does not exist.", Status: http.StatusNotFound}
	ErrInvalidPart          = S3Error{Code: "InvalidPart", Message: "One or more of the specified parts could not be found or did not match the supplied ETag.", Status: http.StatusBadRequest}
	ErrInvalidPartOrder     = S3Error{Code: "InvalidPartOrder", Message: "The list of parts was not in ascending order.", Status: http.StatusBadRequest}
	ErrInvalidArgument      = S3Error{Code: "InvalidArgument", Message: "Invalid Argument", Status: http.StatusBadRequest}
	ErrServiceUnavailable   = S3Error{Code: "ServiceUnavailable", Message: "Reduce your request rate.", Status: http.StatusServiceUnavailable}
	ErrInternalError        = S3Error{Code: "InternalError", Message: "We encountered an internal error. Please try again.", Status: http.StatusInternalServerError}
)

func WriteError(w http.ResponseWriter, s3err S3Error, resource, requestID string) {
	writeError(w, s3err, resource, requestID, false)
}

func WriteErrorResponse(w http.ResponseWriter, r *http.Request, s3err S3Error, resource, requestID string) {
	writeError(w, s3err, resource, requestID, r != nil && r.Method == http.MethodHead)
}

func writeError(w http.ResponseWriter, s3err S3Error, resource, requestID string, suppressBody bool) {
	if s3err.Status == 0 {
		s3err = ErrInternalError
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Encoding", "identity")
	if suppressBody {
		w.WriteHeader(s3err.Status)
		return
	}
	body, _ := xml.Marshal(ErrorResponse{
		Code:      s3err.Code,
		Message:   s3err.Message,
		Resource:  resource,
		RequestID: requestID,
	})
	w.Header().Set("Content-Length", strconv.Itoa(len(xml.Header)+len(body)))
	w.WriteHeader(s3err.Status)
	_, _ = io.WriteString(w, xml.Header)
	_, _ = w.Write(body)
}

func MapError(err error) S3Error {
	switch {
	case err == nil:
		return ErrInternalError
	case errors.Is(err, store.ErrNotImplemented):
		return ErrNotImplemented
	case errors.Is(err, store.ErrEntityTooLarge):
		return ErrEntityTooLarge
	case errors.Is(err, ErrInvalidAccessKeyID):
		return ErrInvalidAccessKey
	case errors.Is(err, ErrSignatureDoesNotMatch):
		return ErrSignatureMismatch
	case errors.Is(err, store.ErrNoSuchBucket):
		return ErrNoSuchBucket
	case errors.Is(err, store.ErrNoSuchKey):
		return ErrNoSuchKey
	case errors.Is(err, store.ErrInvalidRange):
		return ErrInvalidRange
	case errors.Is(err, store.ErrMissingContentLength):
		return ErrMissingContentLength
	case errors.Is(err, store.ErrNoSuchUpload):
		return ErrNoSuchUpload
	case errors.Is(err, store.ErrInvalidPart):
		return ErrInvalidPart
	case errors.Is(err, store.ErrInvalidPartOrder):
		return ErrInvalidPartOrder
	case errors.Is(err, store.ErrInvalidArgument):
		return ErrInvalidArgument
	case errors.Is(err, ErrInvalidArgumentValue) || err.Error() == "invalid argument":
		return ErrInvalidArgument
	case errors.Is(err, ErrServiceUnavailableValue) || err.Error() == "service unavailable":
		return ErrServiceUnavailable
	default:
		return ErrInternalError
	}
}

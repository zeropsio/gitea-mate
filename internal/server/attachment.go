package server

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/zeropsio/gitea-mate/internal/gitea"
)

// GET /person/attachments/{uuid} — a picture on a pull request, for the app.
//
// A Mate describes its change as its pull request's body, and the pictures
// that show the change are the request's attachments. Gitea serves an
// attachment's bytes on one web route, /attachments/{uuid}, and a private one
// only to a token that may read it; but the app reads Gitea from another
// origin with the person's token in a header, and Gitea answers the preflight
// that header needs with a redirect to its sign-in page (measured on 1.27.2).
// A token in the query string gets past the preflight and lands, whole, in
// Gitea's request log. So the app asks here instead.
//
// The broker adds nothing of its own: the person's token is forwarded to
// Gitea as it came, Gitea decides what it may read, and the answer is relayed.
// It serves raster pictures and nothing else. A text file, and an SVG, which
// can carry script, are refused rather than served from the broker's own
// origin, and every picture goes out with nosniff and a sandbox.

// maxPictureBytes bounds a picture the route relays. It is a var so a test
// can narrow it.
var maxPictureBytes int64 = 20 << 20

// pictureTypes are what the route serves, as Gitea names them.
var pictureTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
	"image/avif": true,
}

func (s *Server) handlePersonAttachment(w http.ResponseWriter, r *http.Request) {
	s.attachmentCORS(w)
	uuid := r.PathValue("uuid")
	if !gitea.AttachmentPattern.MatchString(uuid) {
		WriteError(w, http.StatusBadRequest, "not_an_attachment", "that is not an attachment's id")
		return
	}
	token := bearerOf(r)
	if token == "" {
		WriteError(w, http.StatusUnauthorized, "gitea_token_required",
			"send the person's Gitea token as the bearer")
		return
	}

	att, err := s.deps.Gitea.AsToken(token).Attachment(r.Context(), uuid)
	switch status := gitea.Status(err); {
	case err == nil:
	case status == http.StatusUnauthorized:
		WriteError(w, http.StatusUnauthorized, "gitea_token_refused", "Gitea did not accept that token")
		return
	case status == http.StatusForbidden:
		WriteError(w, http.StatusForbidden, "forbidden", "that token may not read attachments")
		return
	case status == http.StatusNotFound:
		WriteError(w, http.StatusNotFound, "not_found", "there is no such attachment, or none that token may read")
		return
	default:
		// The error names the path and the status Gitea gave, never the
		// token: the client holds it in a header only.
		s.log.Warn("an attachment could not be read from Gitea", "uuid", uuid, "err", err.Error())
		writeUnavailable(w, "gitea", "Gitea could not be reached")
		return
	}
	defer att.Body.Close()

	mediaType, _, err := mime.ParseMediaType(att.ContentType)
	if err != nil || !pictureTypes[mediaType] {
		WriteError(w, http.StatusUnsupportedMediaType, "not_a_picture",
			"only a picture is served here: png, jpeg, gif, webp or avif")
		return
	}
	body, length, ok := boundedPicture(att)
	if !ok {
		WriteError(w, http.StatusRequestEntityTooLarge, "too_large",
			"that attachment is larger than a picture is served here")
		return
	}

	header := w.Header()
	header.Set("Content-Type", mediaType)
	header.Set("Content-Length", strconv.FormatInt(length, 10))
	header.Set("Content-Disposition", "inline")
	header.Set("Cache-Control", "private, max-age=3600")
	header.Set("Vary", "Authorization, Origin")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// boundedPicture is the attachment's body and its length, when it is no larger
// than maxPictureBytes. Gitea says the length of an attachment it serves; one
// that does not is read up to one byte past the bound, so nothing larger is
// ever sent in part.
func boundedPicture(att *gitea.AttachmentBody) (io.Reader, int64, bool) {
	if att.ContentLength >= 0 {
		if att.ContentLength > maxPictureBytes {
			return nil, 0, false
		}
		return io.LimitReader(att.Body, att.ContentLength), att.ContentLength, true
	}
	raw, err := io.ReadAll(io.LimitReader(att.Body, maxPictureBytes+1))
	if err != nil || int64(len(raw)) > maxPictureBytes {
		return nil, 0, false
	}
	return bytes.NewReader(raw), int64(len(raw)), true
}

func (s *Server) handlePersonAttachmentPreflight(w http.ResponseWriter, _ *http.Request) {
	s.attachmentCORS(w)
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

// attachmentCORS answers every origin, as appCORS does for POST /person/token
// and Gitea's own API does: the proof is the token in the header, and no
// cookie is involved.
func (s *Server) attachmentCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
}

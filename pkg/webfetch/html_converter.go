package webfetch

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/PuerkitoBio/goquery"
	"go.uber.org/zap"
	"golang.org/x/net/html/charset"
)

// htmlToMarkdown converts HTML bytes to Markdown.
func htmlToMarkdown(htmlContent []byte) (string, error) {
	md, err := htmltomarkdown.ConvertReader(bytes.NewReader(htmlContent))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrConversionFailed, err)
	}
	return string(md), nil
}

// stripHTML removes HTML tags, returning plain text (fallback).
func stripHTML(htmlContent []byte) string {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(htmlContent))
	if err != nil {
		return string(htmlContent)
	}
	return doc.Text()
}

// normalizeToUTF8 converts body to UTF-8 based on the declared charset.
// paramName charsetName avoids collision with the charset package name.
func normalizeToUTF8(body []byte, charsetName string, logger *zap.Logger) []byte {
	if charsetName == "" || strings.EqualFold(charsetName, "utf-8") {
		return body
	}
	encoding, _ := charset.Lookup(charsetName)
	if encoding == nil {
		if logger != nil {
			logger.Warn("unsupported charset, returning raw bytes",
				zap.String("charset", charsetName))
		}
		return body
	}
	reader := encoding.NewDecoder().Reader(bytes.NewReader(body))
	result, err := io.ReadAll(reader)
	if err != nil {
		if logger != nil {
			logger.Warn("charset decoding failed, returning raw bytes",
				zap.String("charset", charsetName), zap.Error(err))
		}
		return body
	}
	return result
}

// detectCharset extracts the charset parameter from a Content-Type header.
func detectCharset(header http.Header) string {
	contentType := header.Get("Content-Type")
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return params["charset"]
}

package webfetch

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/ledongthuc/pdf"
	"go.uber.org/zap"
)

// pdfExtractor handles PDF to text extraction.
type pdfExtractor struct {
	logger *zap.Logger
}

// newPDFExtractor creates a new PDF extractor.
func newPDFExtractor(logger *zap.Logger) *pdfExtractor {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &pdfExtractor{logger: logger}
}

// extractText extracts text content from PDF bytes.
// Uses ledongthuc/pdf to extract text from all pages.
func (e *pdfExtractor) extractText(pdfBytes []byte) (string, error) {
	if len(pdfBytes) == 0 {
		return "", fmt.Errorf("empty PDF data")
	}

	// Create a PDF reader from bytes.
	r, err := pdf.NewReader(bytes.NewReader(pdfBytes), int64(len(pdfBytes)))
	if err != nil {
		return "", fmt.Errorf("failed to read PDF: %w", err)
	}

	// Extract text from all pages.
	var textBuilder strings.Builder
	numPages := r.NumPage()

	for pageNum := 1; pageNum <= numPages; pageNum++ {
		p := r.Page(pageNum)
		if p.V.IsNull() {
			continue
		}

		// Extract text rows from the page.
		rows, _ := p.GetTextByRow()
		for _, row := range rows {
			for _, word := range row.Content {
				textBuilder.WriteString(word.S)
				textBuilder.WriteString(" ")
			}
			textBuilder.WriteString("\n")
		}

		// Add page separator.
		if pageNum < numPages {
			textBuilder.WriteString("\n--- Page Break ---\n\n")
		}
	}

	text := textBuilder.String()

	// Clean up the extracted text.
	text = e.cleanExtractedText(text)

	if text == "" {
		return "", fmt.Errorf("no text content found in PDF")
	}

	e.logger.Debug("PDF text extraction completed",
		zap.Int("pages", numPages),
		zap.Int("chars", len(text)),
	)

	return text, nil
}

// cleanExtractedText removes excessive whitespace and normalizes the text.
func (e *pdfExtractor) cleanExtractedText(text string) string {
	// Remove excessive newlines (more than 2 consecutive).
	for strings.Contains(text, "\n\n\n") {
		text = strings.ReplaceAll(text, "\n\n\n", "\n\n")
	}

	// Remove trailing whitespace from each line.
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	text = strings.Join(lines, "\n")

	// Trim leading/trailing whitespace.
	text = strings.TrimSpace(text)

	return text
}

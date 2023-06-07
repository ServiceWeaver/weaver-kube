// Copyright 2023 Google LLC
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

package impl

import (
	"crypto/sha256"
	"fmt"
	"math"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/util/validation"
)

// TODO(rgrandl): Remove duplicate file.

const (
	// Maximum number of components allowed in the name.
	maxNumComponents = 30

	// Minimum length of a sanitized string.
	minSanitizedLen = 1
)

func init() {
	// Enforce the following invariant on maxNumComponents: if all
	// maxNumComponents component names are provided, we guarantee that
	// we can always construct a valid Kubernetes name.
	const n = maxNumComponents + 1           // one extra component for hash
	const minLen = n*minSanitizedLen + n - 1 // components plus '-' separators
	if minLen > validation.DNS1035LabelMaxLength {
		panic(fmt.Sprintf("bad invariant: type Name cannot support %d components", maxNumComponents))
	}
}

// name represents a Kubernetes object name, consisting of N component names
// concatenated using the '-' character:
//
//	<component1>-<component2>-...-<componentN>
//
// Component names are arbitrary strings that need not adhere to Kubernetes's
// strict naming convention, which excludes most of the non-alphanumerical
// symbols including '.' and '_'. Likewise, the combined length of the
// component names need not meet the length requirement for Kubernetes object
// names, which can be as low as 63 characters. Instead, when creating object
// names (using DNSLabel() and DNSSubdomain() methods), the component names
// are sanitized in a way that preserves as much of their information as
// possible, while satisfying the Kubernetes naming requirements for the
// concatenated string.
//
// A simplistic sanitization, however, can lead to different names being
// converted to the same name (e.g., "foo_bar" and "foo.bar" may both be
// converted to "foobar"), leading to collisions.  To prevent these
// collisions, we append a combined hash of all original components to the
// object name:
//
//	<sanitizedComponent1>-<sanitizedComponent2>-...-<sanitizedComponentN>-<hash>
//
// Empty component names are dropped and not concatenated.
type name [maxNumComponents]string

// DNSLabel returns a human readable name that follows the DNS label
// standard as defined in RFC 1035.
func (n name) DNSLabel() string {
	return n.convert(true /*isLabel*/)
}

// DNSSubdomain returns a human-readable name that follows the DNS
// subdomain standard as defined in RFC 1123.
func (n name) DNSSubdomain() string {
	return n.convert(false /*isLabel*/)
}

func (n name) convert(isLabel bool) string {
	var maxTotalLen int // max length for the entire name
	if isLabel {
		maxTotalLen = validation.DNS1035LabelMaxLength
	} else {
		maxTotalLen = validation.DNS1123SubdomainMaxLength
	}

	// Create a list of non-empty components.
	var components []string
	for _, c := range n {
		if c != "" {
			components = append(components, c)
		}
	}

	// Compute a consistent hash of all of the components and append it
	// to the list of components.
	hash := hash8(components)
	components = append(components, hash)

	// Maximum length for all component names combined.
	maxCombinedLen := maxTotalLen
	if len(components) > 1 {
		// Account for the '-' separators between components.
		maxCombinedLen -= (len(components) - 1)
	}

	// Step 1: Sanitize all components without any length limits. This gives us
	// the upper bound on the sanitized length for each component.
	var maxComponentLength [maxNumComponents]int
	for i := 0; i < len(components); i++ {
		name := sanitizer{
			maxLen:     math.MaxInt32,
			numStartOK: i > 0 || !isLabel,
			dotOK:      !isLabel,
		}.sanitize(components[i])
		maxComponentLength[i] = len(name)
	}

	// Step 2: Allocate an equal target length T for each component.
	T := maxCombinedLen / len(components)
	if T < minSanitizedLen {
		panic(fmt.Sprintf("impossible: target component length %d can never be less than %d due to an invariant", T, minSanitizedLen))
	}

	// Step 3: Sanitize all components whose max sanitized length is less than
	// or equal to T. Collect all of the unused bytes into a remainder value R.
	var R int
	for i := 0; i < len(components); i++ {
		if maxComponentLength[i] > T {
			continue
		}
		name := sanitizer{
			maxLen:     T,
			numStartOK: i > 0 || !isLabel,
			dotOK:      !isLabel,
		}.sanitize(components[i])
		components[i] = name
		R += T - len(name)
	}

	// Step 4: Sanitize all other components, using the remainder budget R.
	for i := 0; i < len(components); i++ {
		if maxComponentLength[i] <= T { // already sanitized
			continue
		}
		name := sanitizer{
			maxLen:     T + R,
			numStartOK: i > 0 || !isLabel,
			dotOK:      !isLabel,
		}.sanitize(components[i])
		components[i] = name
		R += T - len(name)
	}

	return strings.Join(components, "-")
}

// isAplhanum returns true iff r is in the character set [a-zA-Z0-9].
func isAlphanum(r rune) bool {
	return isAlpha(r) || (r >= '0' && r <= '9')
}

// isAlpha returns true iff r is in the character set [a-zA-Z].
func isAlpha(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

type sanitizer struct {
	maxLen     int
	numStartOK bool
	dotOK      bool
}

// sanitize returns a human-readable variant of name that is a valid
// DNS name and whose length:
//   - Greater than zero.
//   - Less than or equal to maxLen and len(name).
//
// If s.numStartOK is false, the returned variant will start with a letter.
// If s.dotOK is true, the returned variant may include '.' characters.
//
// REQUIRES: !name.empty()
func (s sanitizer) sanitize(name string) string {
	runes := []rune(name)

	// Remove/replace all runes not in the set [-.a-zA-Z0-9].
	idx := 0
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '.':
			if !s.dotOK {
				r = '-'
			}
		case r == '_' || r == '-':
			r = '-'
		case r == '/':
			if s.dotOK {
				r = '.'
			} else {
				r = '-'
			}
		case isAlphanum(r):
			r = unicode.ToLower(r)
		default:
			continue // drop the rune
		}

		if !isAlphanum(r) && idx > 0 && !isAlphanum(runes[idx-1]) {
			// Don't keep adjacent non-alphanumeric characters.
			continue
		}
		runes[idx] = r
		idx++
	}
	runes = runes[:idx]

	// Shorten the runes length, if necessary. We remove the prefix, since
	// the suffix carries more information (e.g., object type).
	if len(runes) > s.maxLen {
		runes = runes[len(runes)-s.maxLen:]
	}

	// Ensure that the start and the end runes are alphanumeric characters.
	// If necessary, ensure that the start rune is an alphabetic character.
	start := 0
	for ; start < len(runes); start++ {
		r := runes[start]
		if isAlpha(r) || (s.numStartOK && isAlphanum(r)) {
			break
		}
	}
	runes = runes[start:]
	end := len(runes) - 1
	for ; end >= 0 && !isAlphanum(runes[end]); end-- {
	}
	runes = runes[:end+1]

	// Ensure a non-empty set of runes remains.
	if len(runes) == 0 {
		runes = []rune{'a'}
	}

	return string(runes)
}

// hash computes a stable 8-byte hash over the provided strings.
func hash8(strs []string) string {
	h := sha256.New()
	var data []byte
	for _, str := range strs {
		h.Write([]byte(str))
		h.Write(data)
		data = h.Sum(data)
	}
	return uuid.NewHash(h, uuid.Nil, data, 0).String()[:8]
}

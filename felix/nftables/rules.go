// Copyright (c) 2016-2022 Tigera, Inc. All rights reserved.
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

package nftables

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/calico/felix/environment"
	"sigs.k8s.io/knftables"
)

const (
	// Compromise: shorter is better for table occupancy and readability. Longer is better for
	// collision-resistance.  16 chars gives us 96 bits of entropy, which is fairly collision
	// resistant.
	HashLength = 16
)

type Rule struct {
	Match   MatchCriteria
	Action  Action
	Comment []string
}

func (r Rule) Render(chain string, prefixFragment string, features *environment.Features) *knftables.Rule {
	return &knftables.Rule{
		Chain:   chain,
		Rule:    r.renderInner([]string{}, "", features),
		Comment: r.comment(prefixFragment),
	}
}

func (r Rule) RenderAppend(table, chainName, prefixFragment string, features *environment.Features) string {
	fragments := make([]string, 0, 6)
	fragments = append(fragments, "add rule", table, chainName)
	return r.renderInner(fragments, prefixFragment, features)
}

func (r Rule) RenderInsert(table, chainName, prefixFragment string, features *environment.Features) string {
	fragments := make([]string, 0, 6)
	fragments = append(fragments, "insert rule", table, chainName)
	return r.renderInner(fragments, prefixFragment, features)
}

func (r Rule) RenderInsertAtRuleNumber(table, chainName string, ruleNum int, prefixFragment string, features *environment.Features) string {
	fragments := make([]string, 0, 7)
	fragments = append(fragments, "insert rule", table, chainName, fmt.Sprintf("position %d", ruleNum))
	return r.renderInner(fragments, prefixFragment, features)
}

// RenderReplace renders the rule as a replacement for the rule at the given handle in the given chain.
// handle can be found using "nft -a list table <table>"
func (r Rule) RenderReplace(table, chainName string, handle int, prefixFragment string, features *environment.Features) string {
	fragments := make([]string, 0, 7)
	fragments = append(fragments, "replace rule", table, chainName, fmt.Sprintf("handle %d", handle))
	return r.renderInner(fragments, prefixFragment, features)
}

func (r Rule) comment(prefixFragment string) *string {
	fragments := []string{}
	for _, c := range r.Comment {
		c = escapeComment(c)
		c = truncateComment(c)
		commentFragment := fmt.Sprintf("%s; %s", prefixFragment, c)
		fragments = append(fragments, commentFragment)
	}
	cmt := strings.Join(fragments, " ")
	if cmt == "" {
		return nil
	}
	return &cmt
}

func (r Rule) renderInner(fragments []string, prefixFragment string, features *environment.Features) string {
	if prefixFragment != "" {
		fragments = append(fragments, prefixFragment)
	}
	matchFragment := r.Match.Render()
	if matchFragment != "" {
		fragments = append(fragments, matchFragment)
	}
	if r.Action != nil {
		actionFragment := r.Action.ToFragment(features)
		if actionFragment != "" {
			fragments = append(fragments, actionFragment)
		}
	}
	inner := strings.Join(fragments, " ")
	if len(inner) == 0 {
		// If the rule is empty, it will cause nft to fail with a cryptic error message.
		// Instead, we'll just use a counter.
		return "counter"
	}
	return inner
}

var shellUnsafe = regexp.MustCompile(`[^\w @%+=:,./-]`)

// escapeComment replaces anything other than "safe" shell characters with an
// underscore (_).  This is a lossy conversion, but the expected use case
// for this stuff getting all the way to iptables are either
//   - hashes/IDs generated by higher layer systems
//   - comments on what the rules do
//
// which should be fine with this limitation.
// There just isn't a good way to escape this stuff in a way that iptables-restore
// will respect.  strconv.Quote() leaves actual quote characters in the output,
// which break iptables-restore.
func escapeComment(s string) string {
	return shellUnsafe.ReplaceAllString(s, "_")
}

const maxCommentLen = 256

func truncateComment(s string) string {
	if len(s) > maxCommentLen {
		return s[0:maxCommentLen]
	}
	return s
}

type Chain struct {
	Name  string
	Rules []Rule
}

func (c *Chain) MakeChain() knftables.Chain {
	return knftables.Chain{}
}

func (c *Chain) RuleHashes(features *environment.Features) []string {
	if c == nil {
		return nil
	}
	hashes := make([]string, len(c.Rules))
	// First hash the chain name so that identical rules in different chains will get different
	// hashes.
	s := sha256.New224()
	_, err := s.Write([]byte(c.Name))
	if err != nil {
		log.WithFields(log.Fields{
			"chain": c.Name,
		}).WithError(err).Panic("Failed to write suffix to hash.")
		return nil
	}

	hash := s.Sum(nil)
	for ii, rule := range c.Rules {
		// Each hash chains in the previous hash, so that its position in the chain and
		// the rules before it affect its hash.
		s.Reset()
		_, err = s.Write(hash)
		if err != nil {
			log.WithFields(log.Fields{
				"action":   rule.Action,
				"position": ii,
				"chain":    c.Name,
			}).WithError(err).Panic("Failed to write suffix to hash.")
		}
		ruleForHashing := rule.RenderAppend("", c.Name, "HASH", features) // TODO: CASEY: Empty table name OK?
		_, err = s.Write([]byte(ruleForHashing))
		if err != nil {
			log.WithFields(log.Fields{
				"ruleFragment": ruleForHashing,
				"action":       rule.Action,
				"position":     ii,
				"chain":        c.Name,
			}).WithError(err).Panic("Failed to write rule for hashing.")
		}
		hash = s.Sum(hash[0:0])
		// Encode the hash using a compact character set.  We use the URL-safe base64
		// variant because it uses '-' and '_', which are more shell-friendly.
		hashes[ii] = base64.RawURLEncoding.EncodeToString(hash)[:HashLength]
		if log.GetLevel() >= log.DebugLevel {
			log.WithFields(log.Fields{
				"ruleFragment": ruleForHashing,
				"action":       rule.Action,
				"position":     ii,
				"chain":        c.Name,
				"hash":         hashes[ii],
			}).Debug("Hashed rule")
		}
	}
	return hashes
}

func (c *Chain) IPSetNames() (ipSetNames []string) {
	if c == nil {
		return nil
	}
	for _, rule := range c.Rules {
		ipSetNames = append(ipSetNames, rule.Match.IPSetNames()...)
	}
	return
}

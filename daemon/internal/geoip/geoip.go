// Package geoip reads the country database shipped in the image and
// answers two questions: which country an address is in, and which
// address ranges make up a country.
//
// The file is DB-IP's "IP to Country Lite" CSV, gzip-compressed, exactly
// as published: one "first,last,CC" range per line, IPv4 and IPv6 mixed,
// sorted. It is licensed CC BY 4.0 (https://db-ip.com).
package geoip

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
)

// DefaultPath is where the image build puts the database.
const DefaultPath = "/usr/share/porter/geoip.csv.gz"

// Ranges are kept in compact form: the full database is several hundred
// thousand entries.
type entry4 struct {
	first, last uint32
	cc          [2]byte
}

type entry6 struct {
	first, last [16]byte
	cc          [2]byte
}

type DB struct {
	v4 []entry4
	v6 []entry6
	// Date is the month of publication ("2026-11"), if known.
	Date string

	mu    sync.Mutex
	cache map[string][2][]string // country -> nft range strings (v4, v6)
}

func u32(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func addr4(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// Load reads a database file. The date is taken from a sibling file
// "<path>.date" when present.
func Load(path string) (*DB, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	db, err := Parse(r)
	if err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(path + ".date"); err == nil {
		db.Date = strings.TrimSpace(string(b))
	}
	return db, nil
}

// Parse reads the CSV form.
func Parse(r io.Reader) (*DB, error) {
	db := &DB{cache: map[string][2][]string{}}
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		f := strings.Split(strings.TrimSpace(sc.Text()), ",")
		if len(f) < 3 || len(f[2]) != 2 {
			continue
		}
		first, err1 := netip.ParseAddr(f[0])
		last, err2 := netip.ParseAddr(f[1])
		if err1 != nil || err2 != nil || first.Is4() != last.Is4() || last.Less(first) {
			return nil, fmt.Errorf("geoip: bad range on line %d", line)
		}
		cc := [2]byte{f[2][0], f[2][1]}
		if first.Is4() {
			db.v4 = append(db.v4, entry4{u32(first), u32(last), cc})
		} else {
			db.v6 = append(db.v6, entry6{first.As16(), last.As16(), cc})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(db.v4) == 0 && len(db.v6) == 0 {
		return nil, fmt.Errorf("geoip: no ranges found")
	}
	sort.Slice(db.v4, func(i, j int) bool { return db.v4[i].first < db.v4[j].first })
	sort.Slice(db.v6, func(i, j int) bool { return bytes.Compare(db.v6[i].first[:], db.v6[j].first[:]) < 0 })
	return db, nil
}

// Lookup returns the country code of an address, "" when unknown.
func (db *DB) Lookup(a netip.Addr) string {
	if db == nil || !a.IsValid() {
		return ""
	}
	a = a.Unmap()
	var cc [2]byte
	if a.Is4() {
		v := u32(a)
		// First range starting after v; the candidate is the one before.
		i := sort.Search(len(db.v4), func(i int) bool { return db.v4[i].first > v })
		if i == 0 || db.v4[i-1].last < v {
			return ""
		}
		cc = db.v4[i-1].cc
	} else {
		v := a.As16()
		i := sort.Search(len(db.v6), func(i int) bool { return bytes.Compare(db.v6[i].first[:], v[:]) > 0 })
		if i == 0 || bytes.Compare(db.v6[i-1].last[:], v[:]) < 0 {
			return ""
		}
		cc = db.v6[i-1].cc
	}
	if cc == [2]byte{'Z', 'Z'} {
		return ""
	}
	return string(cc[:])
}

// Ranges returns a country's address ranges in nftables notation
// ("a-b", or a single address), adjacent ranges merged.
func (db *DB) Ranges(country string) (v4, v6 []string) {
	if db == nil || len(country) != 2 {
		return nil, nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if c, ok := db.cache[country]; ok {
		return c[0], c[1]
	}
	want := [2]byte{country[0], country[1]}
	var out []string
	var first, last netip.Addr
	flush := func() {
		if !first.IsValid() {
			return
		}
		if first == last {
			out = append(out, first.String())
		} else {
			out = append(out, first.String()+"-"+last.String())
		}
		first = netip.Addr{}
	}
	add := func(f, l netip.Addr) {
		if first.IsValid() && last.Next() == f {
			last = l
			return
		}
		flush()
		first, last = f, l
	}
	for _, e := range db.v4 {
		if e.cc == want {
			add(addr4(e.first), addr4(e.last))
		}
	}
	flush()
	v4, out = out, nil
	for _, e := range db.v6 {
		if e.cc == want {
			add(netip.AddrFrom16(e.first), netip.AddrFrom16(e.last))
		}
	}
	flush()
	v6 = out
	db.cache[country] = [2][]string{v4, v6}
	return v4, v6
}

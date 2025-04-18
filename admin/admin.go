package admin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mjl-/mox/config"
	"github.com/mjl-/mox/dns"
	"github.com/mjl-/mox/junk"
	"github.com/mjl-/mox/mlog"
	"github.com/mjl-/mox/mox-"
	"github.com/mjl-/mox/mtasts"
	"github.com/mjl-/mox/queue"
	"github.com/mjl-/mox/smtp"
	"github.com/mjl-/mox/store"
)

var pkglog = mlog.New("admin", nil)

var ErrRequest = errors.New("bad request")

// MakeDKIMEd25519Key returns a PEM buffer containing an ed25519 key for use
// with DKIM.
// selector and domain can be empty. If not, they are used in the note.
func MakeDKIMEd25519Key(selector, domain dns.Domain) ([]byte, error) {
	_, privKey, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating key: %w", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}

	block := &pem.Block{
		Type: "PRIVATE KEY",
		Headers: map[string]string{
			"Note": dkimKeyNote("ed25519", selector, domain),
		},
		Bytes: pkcs8,
	}
	b := &bytes.Buffer{}
	if err := pem.Encode(b, block); err != nil {
		return nil, fmt.Errorf("encoding pem: %w", err)
	}
	return b.Bytes(), nil
}

func dkimKeyNote(kind string, selector, domain dns.Domain) string {
	s := kind + " dkim private key"
	var zero dns.Domain
	if selector != zero && domain != zero {
		s += fmt.Sprintf(" for %s._domainkey.%s", selector.ASCII, domain.ASCII)
	}
	s += fmt.Sprintf(", generated by mox on %s", time.Now().Format(time.RFC3339))
	return s
}

// MakeDKIMRSAKey returns a PEM buffer containing an rsa key for use with
// DKIM.
// selector and domain can be empty. If not, they are used in the note.
func MakeDKIMRSAKey(selector, domain dns.Domain) ([]byte, error) {
	// 2048 bits seems reasonable in 2022, 1024 is on the low side, larger
	// keys may not fit in UDP DNS response.
	privKey, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generating key: %w", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}

	block := &pem.Block{
		Type: "PRIVATE KEY",
		Headers: map[string]string{
			"Note": dkimKeyNote("rsa-2048", selector, domain),
		},
		Bytes: pkcs8,
	}
	b := &bytes.Buffer{}
	if err := pem.Encode(b, block); err != nil {
		return nil, fmt.Errorf("encoding pem: %w", err)
	}
	return b.Bytes(), nil
}

// MakeAccountConfig returns a new account configuration for an email address.
func MakeAccountConfig(addr smtp.Address) config.Account {
	account := config.Account{
		Domain: addr.Domain.Name(),
		Destinations: map[string]config.Destination{
			addr.String(): {},
		},
		RejectsMailbox: "Rejects",
		JunkFilter: &config.JunkFilter{
			Threshold: 0.95,
			Params: junk.Params{
				Onegrams:    true,
				MaxPower:    .01,
				TopWords:    10,
				IgnoreWords: .1,
				RareWords:   2,
			},
		},
		NoCustomPassword: true,
	}
	account.AutomaticJunkFlags.Enabled = true
	account.AutomaticJunkFlags.JunkMailboxRegexp = "^(junk|spam)"
	account.AutomaticJunkFlags.NeutralMailboxRegexp = "^(inbox|neutral|postmaster|dmarc|tlsrpt|rejects)"
	account.SubjectPass.Period = 12 * time.Hour
	return account
}

func writeFile(log mlog.Log, path string, data []byte) error {
	os.MkdirAll(filepath.Dir(path), 0770)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0660)
	if err != nil {
		return fmt.Errorf("creating file %s: %s", path, err)
	}
	defer func() {
		if f != nil {
			err := f.Close()
			log.Check(err, "closing file after error")
			err = os.Remove(path)
			log.Check(err, "removing file after error", slog.String("path", path))
		}
	}()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing file %s: %s", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close file: %v", err)
	}
	f = nil
	return nil
}

// MakeDomainConfig makes a new config for a domain, creating DKIM keys, using
// accountName for DMARC and TLS reports.
func MakeDomainConfig(ctx context.Context, domain, hostname dns.Domain, accountName string, withMTASTS bool) (config.Domain, []string, error) {
	log := pkglog.WithContext(ctx)

	now := time.Now()
	year := now.Format("2006")
	timestamp := now.Format("20060102T150405")

	var paths []string
	defer func() {
		for _, p := range paths {
			err := os.Remove(p)
			log.Check(err, "removing path for domain config", slog.String("path", p))
		}
	}()

	confDKIM := config.DKIM{
		Selectors: map[string]config.Selector{},
	}

	addSelector := func(kind, name string, privKey []byte) error {
		record := fmt.Sprintf("%s._domainkey.%s", name, domain.ASCII)
		keyPath := filepath.Join("dkim", fmt.Sprintf("%s.%s.%s.privatekey.pkcs8.pem", record, timestamp, kind))
		p := mox.ConfigDynamicDirPath(keyPath)
		if err := writeFile(log, p, privKey); err != nil {
			return err
		}
		paths = append(paths, p)
		confDKIM.Selectors[name] = config.Selector{
			// Example from RFC has 5 day between signing and expiration. ../rfc/6376:1393
			// Expiration is not intended as antireplay defense, but it may help. ../rfc/6376:1340
			// Messages in the wild have been observed with 2 hours and 1 year expiration.
			Expiration:     "72h",
			PrivateKeyFile: keyPath,
		}
		return nil
	}

	addRSA := func(name string) error {
		key, err := MakeDKIMRSAKey(dns.Domain{ASCII: name}, domain)
		if err != nil {
			return fmt.Errorf("making dkim rsa private key: %s", err)
		}
		return addSelector("rsa2048", name, key)
	}

	if err := addRSA(year + "a"); err != nil {
		return config.Domain{}, nil, err
	}
	if err := addRSA(year + "b"); err != nil {
		return config.Domain{}, nil, err
	}

	// We sign with the first two. In case they are misused, the switch to the other
	// keys is easy, just change the config. Operators should make the public key field
	// of the misused keys empty in the DNS records to disable the misused keys.
	confDKIM.Sign = []string{year + "a"}

	confDomain := config.Domain{
		ClientSettingsDomain:       "mail." + domain.Name(),
		LocalpartCatchallSeparator: "+",
		DKIM:                       confDKIM,
		DMARC: &config.DMARC{
			Account:   accountName,
			Localpart: "dmarcreports",
			Mailbox:   "DMARC",
		},
		TLSRPT: &config.TLSRPT{
			Account:   accountName,
			Localpart: "tlsreports",
			Mailbox:   "TLSRPT",
		},
	}

	if withMTASTS {
		confDomain.MTASTS = &config.MTASTS{
			PolicyID: time.Now().UTC().Format("20060102T150405"),
			Mode:     mtasts.ModeEnforce,
			// We start out with 24 hour, and warn in the admin interface that users should
			// increase it to weeks once the setup works.
			MaxAge: 24 * time.Hour,
			MX:     []string{hostname.ASCII},
		}
	}

	rpaths := paths
	paths = nil

	return confDomain, rpaths, nil
}

// DKIMAdd adds a DKIM selector for a domain, generating a key and writing it to disk.
func DKIMAdd(ctx context.Context, domain, selector dns.Domain, algorithm, hash string, headerRelaxed, bodyRelaxed, seal bool, headers []string, lifetime time.Duration) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("adding dkim key", rerr,
				slog.Any("domain", domain),
				slog.Any("selector", selector))
		}
	}()

	switch hash {
	case "sha256", "sha1":
	default:
		return fmt.Errorf("%w: unknown hash algorithm %q", ErrRequest, hash)
	}

	var privKey []byte
	var err error
	var kind string
	switch algorithm {
	case "rsa":
		privKey, err = MakeDKIMRSAKey(selector, domain)
		kind = "rsa2048"
	case "ed25519":
		privKey, err = MakeDKIMEd25519Key(selector, domain)
		kind = "ed25519"
	default:
		err = fmt.Errorf("unknown algorithm")
	}
	if err != nil {
		return fmt.Errorf("%w: making dkim key: %v", ErrRequest, err)
	}

	// Only take lock now, we don't want to hold it while generating a key.
	defer mox.Conf.DynamicLockUnlock()()

	c := mox.Conf.Dynamic
	d, ok := c.Domains[domain.Name()]
	if !ok {
		return fmt.Errorf("%w: domain does not exist", ErrRequest)
	}

	if _, ok := d.DKIM.Selectors[selector.Name()]; ok {
		return fmt.Errorf("%w: selector already exists for domain", ErrRequest)
	}

	record := fmt.Sprintf("%s._domainkey.%s", selector.ASCII, domain.ASCII)
	timestamp := time.Now().Format("20060102T150405")
	keyPath := filepath.Join("dkim", fmt.Sprintf("%s.%s.%s.privatekey.pkcs8.pem", record, timestamp, kind))
	p := mox.ConfigDynamicDirPath(keyPath)
	if err := writeFile(log, p, privKey); err != nil {
		return fmt.Errorf("writing key file: %v", err)
	}
	removePath := p
	defer func() {
		if removePath != "" {
			err := os.Remove(removePath)
			log.Check(err, "removing path for dkim key", slog.String("path", removePath))
		}
	}()

	nsel := config.Selector{
		Hash: hash,
		Canonicalization: config.Canonicalization{
			HeaderRelaxed: headerRelaxed,
			BodyRelaxed:   bodyRelaxed,
		},
		Headers:         headers,
		DontSealHeaders: !seal,
		Expiration:      lifetime.String(),
		PrivateKeyFile:  keyPath,
	}

	// All good, time to update the config.
	nd := d
	nd.DKIM.Selectors = map[string]config.Selector{}
	for name, osel := range d.DKIM.Selectors {
		nd.DKIM.Selectors[name] = osel
	}
	nd.DKIM.Selectors[selector.Name()] = nsel
	nc := c
	nc.Domains = map[string]config.Domain{}
	for name, dom := range c.Domains {
		nc.Domains[name] = dom
	}
	nc.Domains[domain.Name()] = nd

	if err := mox.WriteDynamicLocked(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %w", err)
	}

	log.Info("dkim key added", slog.Any("domain", domain), slog.Any("selector", selector))
	removePath = "" // Prevent cleanup of key file.
	return nil
}

// DKIMRemove removes the selector from the domain, moving the key file out of the way.
func DKIMRemove(ctx context.Context, domain, selector dns.Domain) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("removing dkim key", rerr,
				slog.Any("domain", domain),
				slog.Any("selector", selector))
		}
	}()

	defer mox.Conf.DynamicLockUnlock()()

	c := mox.Conf.Dynamic
	d, ok := c.Domains[domain.Name()]
	if !ok {
		return fmt.Errorf("%w: domain does not exist", ErrRequest)
	}

	sel, ok := d.DKIM.Selectors[selector.Name()]
	if !ok {
		return fmt.Errorf("%w: selector does not exist for domain", ErrRequest)
	}

	nsels := map[string]config.Selector{}
	for name, sel := range d.DKIM.Selectors {
		if name != selector.Name() {
			nsels[name] = sel
		}
	}
	nsign := make([]string, 0, len(d.DKIM.Sign))
	for _, name := range d.DKIM.Sign {
		if name != selector.Name() {
			nsign = append(nsign, name)
		}
	}

	nd := d
	nd.DKIM = config.DKIM{Selectors: nsels, Sign: nsign}
	nc := c
	nc.Domains = map[string]config.Domain{}
	for name, dom := range c.Domains {
		nc.Domains[name] = dom
	}
	nc.Domains[domain.Name()] = nd

	if err := mox.WriteDynamicLocked(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %w", err)
	}

	// Move away a DKIM private key to a subdirectory "old". But only if
	// not in use by other domains.
	usedKeyPaths := gatherUsedKeysPaths(nc)
	moveAwayKeys(log, map[string]config.Selector{selector.Name(): sel}, usedKeyPaths)

	log.Info("dkim key removed", slog.Any("domain", domain), slog.Any("selector", selector))
	return nil
}

// DomainAdd adds the domain to the domains config, rewriting domains.conf and
// marking it loaded.
//
// accountName is used for DMARC/TLS report and potentially for the postmaster address.
// If the account does not exist, it is created with localpart. Localpart must be
// set only if the account does not yet exist.
func DomainAdd(ctx context.Context, disabled bool, domain dns.Domain, accountName string, localpart smtp.Localpart) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("adding domain", rerr,
				slog.Any("disabled", disabled),
				slog.Any("domain", domain),
				slog.String("account", accountName),
				slog.Any("localpart", localpart))
		}
	}()

	defer mox.Conf.DynamicLockUnlock()()

	c := mox.Conf.Dynamic
	if _, ok := c.Domains[domain.Name()]; ok {
		return fmt.Errorf("%w: domain already present", ErrRequest)
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Domains = map[string]config.Domain{}
	for name, d := range c.Domains {
		nc.Domains[name] = d
	}

	// Only enable mta-sts for domain if there is a listener with mta-sts.
	var withMTASTS bool
	for _, l := range mox.Conf.Static.Listeners {
		if l.MTASTSHTTPS.Enabled {
			withMTASTS = true
			break
		}
	}

	confDomain, cleanupFiles, err := MakeDomainConfig(ctx, domain, mox.Conf.Static.HostnameDomain, accountName, withMTASTS)
	if err != nil {
		return fmt.Errorf("preparing domain config: %v", err)
	}
	defer func() {
		for _, f := range cleanupFiles {
			err := os.Remove(f)
			log.Check(err, "cleaning up file after error", slog.String("path", f))
		}
	}()
	confDomain.Disabled = disabled

	if _, ok := c.Accounts[accountName]; ok && localpart != "" {
		return fmt.Errorf("%w: account already exists (leave localpart empty when using an existing account)", ErrRequest)
	} else if !ok && localpart == "" {
		return fmt.Errorf("%w: account does not yet exist (specify a localpart)", ErrRequest)
	} else if accountName == "" {
		return fmt.Errorf("%w: account name is empty", ErrRequest)
	} else if !ok {
		nc.Accounts[accountName] = MakeAccountConfig(smtp.NewAddress(localpart, domain))
	} else if accountName != mox.Conf.Static.Postmaster.Account {
		nacc := nc.Accounts[accountName]
		nd := map[string]config.Destination{}
		for k, v := range nacc.Destinations {
			nd[k] = v
		}
		pmaddr := smtp.NewAddress("postmaster", domain)
		nd[pmaddr.String()] = config.Destination{}
		nacc.Destinations = nd
		nc.Accounts[accountName] = nacc
	}

	nc.Domains[domain.Name()] = confDomain

	if err := mox.WriteDynamicLocked(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %w", err)
	}
	log.Info("domain added", slog.Any("domain", domain), slog.Bool("disabled", disabled))
	cleanupFiles = nil // All good, don't cleanup.
	return nil
}

// DomainRemove removes domain from the config, rewriting domains.conf.
//
// No accounts are removed, also not when they still reference this domain.
func DomainRemove(ctx context.Context, domain dns.Domain) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("removing domain", rerr, slog.Any("domain", domain))
		}
	}()

	defer mox.Conf.DynamicLockUnlock()()

	c := mox.Conf.Dynamic
	domConf, ok := c.Domains[domain.Name()]
	if !ok {
		return fmt.Errorf("%w: domain does not exist", ErrRequest)
	}

	// Check that the domain isn't referenced in a TLS public key.
	tlspubkeys, err := store.TLSPublicKeyList(ctx, "")
	if err != nil {
		return fmt.Errorf("%w: listing tls public keys: %s", ErrRequest, err)
	}
	atdom := "@" + domain.Name()
	for _, tpk := range tlspubkeys {
		if strings.HasSuffix(tpk.LoginAddress, atdom) {
			return fmt.Errorf("%w: domain is still referenced in tls public key by login address %q of account %q, change or remove it first", ErrRequest, tpk.LoginAddress, tpk.Account)
		}
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Domains = map[string]config.Domain{}
	s := domain.Name()
	for name, d := range c.Domains {
		if name != s {
			nc.Domains[name] = d
		}
	}

	if err := mox.WriteDynamicLocked(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %w", err)
	}

	// Move away any DKIM private keys to a subdirectory "old". But only if
	// they are not in use by other domains.
	usedKeyPaths := gatherUsedKeysPaths(nc)
	moveAwayKeys(log, domConf.DKIM.Selectors, usedKeyPaths)

	log.Info("domain removed", slog.Any("domain", domain))
	return nil
}

func gatherUsedKeysPaths(nc config.Dynamic) map[string]bool {
	usedKeyPaths := map[string]bool{}
	for _, dc := range nc.Domains {
		for _, sel := range dc.DKIM.Selectors {
			usedKeyPaths[filepath.Clean(sel.PrivateKeyFile)] = true
		}
	}
	return usedKeyPaths
}

func moveAwayKeys(log mlog.Log, sels map[string]config.Selector, usedKeyPaths map[string]bool) {
	for _, sel := range sels {
		if sel.PrivateKeyFile == "" || usedKeyPaths[filepath.Clean(sel.PrivateKeyFile)] {
			continue
		}
		src := mox.ConfigDirPath(sel.PrivateKeyFile)
		dst := mox.ConfigDirPath(filepath.Join(filepath.Dir(sel.PrivateKeyFile), "old", filepath.Base(sel.PrivateKeyFile)))
		_, err := os.Stat(dst)
		if err == nil {
			err = fmt.Errorf("destination already exists")
		} else if os.IsNotExist(err) {
			os.MkdirAll(filepath.Dir(dst), 0770)
			err = os.Rename(src, dst)
		}
		if err != nil {
			log.Errorx("renaming dkim private key file for removed domain", err, slog.String("src", src), slog.String("dst", dst))
		}
	}
}

// DomainSave calls xmodify with a shallow copy of the domain config. xmodify
// can modify the config, but must clone all referencing data it changes.
// xmodify may employ panic-based error handling. After xmodify returns, the
// modified config is verified, saved and takes effect.
func DomainSave(ctx context.Context, domainName string, xmodify func(config *config.Domain) error) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("saving domain config", rerr)
		}
	}()

	defer mox.Conf.DynamicLockUnlock()()

	nc := mox.Conf.Dynamic            // Shallow copy.
	dom, ok := nc.Domains[domainName] // dom is a shallow copy.
	if !ok {
		return fmt.Errorf("%w: domain not present", ErrRequest)
	}

	if err := xmodify(&dom); err != nil {
		return err
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc.Domains = map[string]config.Domain{}
	for name, d := range mox.Conf.Dynamic.Domains {
		nc.Domains[name] = d
	}
	nc.Domains[domainName] = dom

	if err := mox.WriteDynamicLocked(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %w", err)
	}

	log.Info("domain saved")
	return nil
}

// ConfigSave calls xmodify with a shallow copy of the dynamic config. xmodify
// can modify the config, but must clone all referencing data it changes.
// xmodify may employ panic-based error handling. After xmodify returns, the
// modified config is verified, saved and takes effect.
func ConfigSave(ctx context.Context, xmodify func(config *config.Dynamic)) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("saving config", rerr)
		}
	}()

	defer mox.Conf.DynamicLockUnlock()()

	nc := mox.Conf.Dynamic // Shallow copy.
	xmodify(&nc)

	if err := mox.WriteDynamicLocked(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %w", err)
	}

	log.Info("config saved")
	return nil
}

// AccountAdd adds an account and an initial address and reloads the configuration.
//
// The new account does not have a password, so cannot yet log in. Email can be
// delivered.
//
// Catchall addresses are not supported for AccountAdd. Add separately with AddressAdd.
func AccountAdd(ctx context.Context, account, address string) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("adding account", rerr, slog.String("account", account), slog.String("address", address))
		}
	}()

	addr, err := smtp.ParseAddress(address)
	if err != nil {
		return fmt.Errorf("%w: parsing email address: %v", ErrRequest, err)
	}

	defer mox.Conf.DynamicLockUnlock()()

	c := mox.Conf.Dynamic
	if _, ok := c.Accounts[account]; ok {
		return fmt.Errorf("%w: account already present", ErrRequest)
	}

	// Ensure the directory does not exist, e.g. due to pending account removal, or an
	// otherwise failed cleanup.
	accountDir := filepath.Join(mox.DataDirPath("accounts"), account)
	if _, err := os.Stat(accountDir); err == nil {
		return fmt.Errorf("%w: account directory %q already/still exists", ErrRequest, accountDir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf(`%w: stat account directory %q, expected "does not exist": %v`, ErrRequest, accountDir, err)
	}

	if err := checkAddressAvailable(addr); err != nil {
		return fmt.Errorf("%w: address not available: %v", ErrRequest, err)
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Accounts = map[string]config.Account{}
	for name, a := range c.Accounts {
		nc.Accounts[name] = a
	}
	nc.Accounts[account] = MakeAccountConfig(addr)

	if err := mox.WriteDynamicLocked(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %w", err)
	}
	log.Info("account added", slog.String("account", account), slog.Any("address", addr))
	return nil
}

// AccountRemove removes an account and reloads the configuration.
func AccountRemove(ctx context.Context, account string) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("adding account", rerr, slog.String("account", account))
		}
	}()

	// Open account now. The deferred Close MUST happen after the dynamic unlock,
	// because during tests the consistency checker takes the same lock.
	acc, err := store.OpenAccount(log, account, false)
	if err != nil {
		return fmt.Errorf("%w: open account: %v", ErrRequest, err)
	}
	defer func() {
		err := acc.SessionsClear(context.Background(), log)
		log.Check(err, "clearing account login sessions")

		err = acc.Close()
		log.Check(err, "closing account after error")
	}()

	// Fail message and webhook deliveries from the queue for this account.
	// Must be before the dynamic lock, since failing a message delivers a DSN to the
	// account. We fail instead of drop because an error can still occur causing us to
	// abort.
	nfailed, err := queue.Fail(ctx, log, queue.Filter{Account: account})
	if nfailed > 0 {
		log.Info("failing queued messages for removed account", slog.Int("count", nfailed))
	}
	if err != nil {
		return fmt.Errorf("failing queued messages for account before removing: %v", err)
	}
	ncanceled, err := queue.HookCancel(ctx, log, queue.HookFilter{Account: account})
	if ncanceled > 0 {
		log.Info("canceling queued webhooks for removed account", slog.Int("count", ncanceled))
	}
	if err != nil {
		return fmt.Errorf("canceling queued webhooks for account before removing: %v", err)
	}

	// Cleanup suppressed addresses for account.
	suppressions, err := queue.SuppressionList(ctx, account)
	if err != nil {
		return fmt.Errorf("listing suppressed addresses for account: %v", err)
	}
	for _, sup := range suppressions {
		addr, err := smtp.ParseAddress(sup.BaseAddress)
		if err != nil {
			return fmt.Errorf("parsing suppressed address %q: %v", sup.BaseAddress, err)
		}
		if err := queue.SuppressionRemove(ctx, account, addr.Path()); err != nil {
			return fmt.Errorf("removing suppression %q for account: %v", sup.BaseAddress, err)
		}
	}

	defer mox.Conf.DynamicLockUnlock()()

	c := mox.Conf.Dynamic

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Accounts = map[string]config.Account{}
	for name, a := range c.Accounts {
		if name != account {
			nc.Accounts[name] = a
		}
	}

	// Write new config file.
	if err := mox.WriteDynamicLocked(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %w", err)
	}

	// Mark files for account for removal as soon as all references have gone.
	if err := acc.Remove(context.Background()); err != nil {
		return fmt.Errorf("account removed from configuration file, but scheduling account directory for removal failed: %v", err)
	}

	log.Info("account marked for removal", slog.String("account", account))
	return nil
}

// checkAddressAvailable checks that the address after canonicalization is not
// already configured, and that its localpart does not contain a catchall
// localpart separator.
//
// Must be called with config lock held.
func checkAddressAvailable(addr smtp.Address) error {
	dc, ok := mox.Conf.Dynamic.Domains[addr.Domain.Name()]
	if !ok {
		return fmt.Errorf("domain does not exist")
	}
	lp := mox.CanonicalLocalpart(addr.Localpart, dc)
	if _, ok := mox.Conf.AccountDestinationsLocked[smtp.NewAddress(lp, addr.Domain).String()]; ok {
		return fmt.Errorf("canonicalized address %s already configured", smtp.NewAddress(lp, addr.Domain))
	}
	for _, sep := range dc.LocalpartCatchallSeparatorsEffective {
		if strings.Contains(string(addr.Localpart), sep) {
			return fmt.Errorf("localpart cannot include domain catchall separator %s", sep)
		}
	}
	if _, ok := dc.Aliases[lp.String()]; ok {
		return fmt.Errorf("address in use as alias")
	}
	return nil
}

// AddressAdd adds an email address to an account and reloads the configuration. If
// address starts with an @ it is treated as a catchall address for the domain.
func AddressAdd(ctx context.Context, address, account string) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("adding address", rerr, slog.String("address", address), slog.String("account", account))
		}
	}()

	defer mox.Conf.DynamicLockUnlock()()

	c := mox.Conf.Dynamic
	a, ok := c.Accounts[account]
	if !ok {
		return fmt.Errorf("%w: account does not exist", ErrRequest)
	}

	var destAddr string
	if strings.HasPrefix(address, "@") {
		d, err := dns.ParseDomain(address[1:])
		if err != nil {
			return fmt.Errorf("%w: parsing domain: %v", ErrRequest, err)
		}
		dname := d.Name()
		destAddr = "@" + dname
		if _, ok := mox.Conf.Dynamic.Domains[dname]; !ok {
			return fmt.Errorf("%w: domain does not exist", ErrRequest)
		} else if _, ok := mox.Conf.AccountDestinationsLocked[destAddr]; ok {
			return fmt.Errorf("%w: catchall address already configured for domain", ErrRequest)
		}
	} else {
		addr, err := smtp.ParseAddress(address)
		if err != nil {
			return fmt.Errorf("%w: parsing email address: %v", ErrRequest, err)
		}

		if err := checkAddressAvailable(addr); err != nil {
			return fmt.Errorf("%w: address not available: %v", ErrRequest, err)
		}
		destAddr = addr.String()
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Accounts = map[string]config.Account{}
	for name, a := range c.Accounts {
		nc.Accounts[name] = a
	}
	nd := map[string]config.Destination{}
	for name, d := range a.Destinations {
		nd[name] = d
	}
	nd[destAddr] = config.Destination{}
	a.Destinations = nd
	nc.Accounts[account] = a

	if err := mox.WriteDynamicLocked(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %w", err)
	}
	log.Info("address added", slog.String("address", address), slog.String("account", account))
	return nil
}

// AddressRemove removes an email address and reloads the configuration.
// Address can be a catchall address for the domain of the form "@<domain>".
//
// If the address is member of an alias, remove it from from the alias, unless it
// is the last member.
func AddressRemove(ctx context.Context, address string) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("removing address", rerr, slog.String("address", address))
		}
	}()

	defer mox.Conf.DynamicLockUnlock()()

	ad, ok := mox.Conf.AccountDestinationsLocked[address]
	if !ok {
		return fmt.Errorf("%w: address does not exists", ErrRequest)
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	a, ok := mox.Conf.Dynamic.Accounts[ad.Account]
	if !ok {
		return fmt.Errorf("internal error: cannot find account")
	}
	na := a
	na.Destinations = map[string]config.Destination{}
	var dropped bool
	for destAddr, d := range a.Destinations {
		if destAddr != address {
			na.Destinations[destAddr] = d
		} else {
			dropped = true
		}
	}
	if !dropped {
		return fmt.Errorf("%w: address not removed, likely a postmaster/reporting address", ErrRequest)
	}

	// Also remove matching address from FromIDLoginAddresses, composing a new slice.
	// Refuse if address is referenced in a TLS public key.
	var dom dns.Domain
	var pa smtp.Address // For non-catchall addresses (most).
	var err error
	if strings.HasPrefix(address, "@") {
		dom, err = dns.ParseDomain(address[1:])
		if err != nil {
			return fmt.Errorf("%w: parsing domain for catchall address: %v", ErrRequest, err)
		}
	} else {
		pa, err = smtp.ParseAddress(address)
		if err != nil {
			return fmt.Errorf("%w: parsing address: %v", ErrRequest, err)
		}
		dom = pa.Domain
	}
	dc, ok := mox.Conf.Dynamic.Domains[dom.Name()]
	if !ok {
		return fmt.Errorf("%w: unknown domain in address %q", ErrRequest, address)
	}

	var fromIDLoginAddresses []string
	for i, fa := range a.ParsedFromIDLoginAddresses {
		if fa.Domain != dom {
			// Keep for different domain.
			fromIDLoginAddresses = append(fromIDLoginAddresses, a.FromIDLoginAddresses[i])
			continue
		}
		if strings.HasPrefix(address, "@") {
			continue
		}
		flp := mox.CanonicalLocalpart(fa.Localpart, dc)
		alp := mox.CanonicalLocalpart(pa.Localpart, dc)
		if alp != flp {
			// Keep for different localpart.
			fromIDLoginAddresses = append(fromIDLoginAddresses, a.FromIDLoginAddresses[i])
		}
	}
	na.FromIDLoginAddresses = fromIDLoginAddresses

	// Refuse if there is still a TLS public key that references this address.
	tlspubkeys, err := store.TLSPublicKeyList(ctx, ad.Account)
	if err != nil {
		return fmt.Errorf("%w: listing tls public keys for account: %v", ErrRequest, err)
	}
	for _, tpk := range tlspubkeys {
		a, err := smtp.ParseAddress(tpk.LoginAddress)
		if err != nil {
			return fmt.Errorf("%w: parsing address from tls public key: %v", ErrRequest, err)
		}
		lp := mox.CanonicalLocalpart(a.Localpart, dc)
		ca := smtp.NewAddress(lp, a.Domain)
		if xad, ok := mox.Conf.AccountDestinationsLocked[ca.String()]; ok && xad.Localpart == ad.Localpart {
			return fmt.Errorf("%w: tls public key %q references this address as login address %q, remove the tls public key before removing the address", ErrRequest, tpk.Fingerprint, tpk.LoginAddress)
		}
	}

	// And remove as member from aliases configured in domains.
	domains := maps.Clone(mox.Conf.Dynamic.Domains)
	for _, aa := range na.Aliases {
		if aa.SubscriptionAddress != address {
			continue
		}

		aliasAddr := fmt.Sprintf("%s@%s", aa.Alias.LocalpartStr, aa.Alias.Domain.Name())

		dom, ok := mox.Conf.Dynamic.Domains[aa.Alias.Domain.Name()]
		if !ok {
			return fmt.Errorf("cannot find domain for alias %s", aliasAddr)
		}
		a, ok := dom.Aliases[aa.Alias.LocalpartStr]
		if !ok {
			return fmt.Errorf("cannot find alias %s", aliasAddr)
		}
		a.Addresses = slices.Clone(a.Addresses)
		a.Addresses = slices.DeleteFunc(a.Addresses, func(v string) bool { return v == address })
		if len(a.Addresses) == 0 {
			return fmt.Errorf("address is last member of alias %s, add new members or remove alias first", aliasAddr)
		}
		a.ParsedAddresses = nil // Filled when parsing config.
		dom.Aliases = maps.Clone(dom.Aliases)
		dom.Aliases[aa.Alias.LocalpartStr] = a
		domains[aa.Alias.Domain.Name()] = dom
	}
	na.Aliases = nil // Filled when parsing config.

	// Check that no message in the queue is for this address. The new account config
	// must still match this address.
	msgs, err := queue.List(ctx, queue.Filter{Account: ad.Account}, queue.Sort{})
	if err != nil {
		return fmt.Errorf("listing messages in queue for account: %v", err)
	}
	for _, m := range msgs {
		dc, ok := mox.Conf.Dynamic.Domains[m.SenderDomainStr]
		if !ok {
			return fmt.Errorf("%w: unknown sender domain %q in queued message", ErrRequest, m.SenderDomainStr)
		}
		lp := mox.CanonicalLocalpart(m.SenderLocalpart, dc)
		sa := smtp.NewAddress(lp, m.SenderDomain.Domain).String()
		if strings.HasPrefix(address, "@") {
			// We are removing the catchall address. The queued message sender address must be
			// configured explicitly to still belong to the account.
			if xad, ok := mox.Conf.AccountDestinationsLocked[sa]; !ok || xad.Account != ad.Account {
				return fmt.Errorf("%w: message delivery queue contains message with sender address %q that depends on the catchall address, drop message from queue first", ErrRequest, sa)
			}
		} else {
			// We are removing a regular address. If the queued message matches the address,
			// the catchall address must be configured for this account.
			if xad, ok := mox.Conf.AccountDestinationsLocked["@"+m.SenderDomainStr]; (!ok || xad.Account != ad.Account) && sa == address {
				return fmt.Errorf("%w: message delivery queue contains message with sender address %q and no catchall address is configured, drop message from queue first", ErrRequest, sa)
			}
		}
	}

	nc := mox.Conf.Dynamic
	nc.Accounts = map[string]config.Account{}
	for name, a := range mox.Conf.Dynamic.Accounts {
		nc.Accounts[name] = a
	}
	nc.Accounts[ad.Account] = na
	nc.Domains = domains

	if err := mox.WriteDynamicLocked(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %w", err)
	}
	log.Info("address removed", slog.String("address", address), slog.String("account", ad.Account))
	return nil
}

func AliasAdd(ctx context.Context, addr smtp.Address, alias config.Alias) error {
	return DomainSave(ctx, addr.Domain.Name(), func(d *config.Domain) error {
		if _, ok := d.Aliases[addr.Localpart.String()]; ok {
			return fmt.Errorf("%w: alias already present", ErrRequest)
		}
		if d.Aliases == nil {
			d.Aliases = map[string]config.Alias{}
		}
		d.Aliases = maps.Clone(d.Aliases)
		d.Aliases[addr.Localpart.String()] = alias
		return nil
	})
}

func AliasUpdate(ctx context.Context, addr smtp.Address, alias config.Alias) error {
	return DomainSave(ctx, addr.Domain.Name(), func(d *config.Domain) error {
		a, ok := d.Aliases[addr.Localpart.String()]
		if !ok {
			return fmt.Errorf("%w: alias does not exist", ErrRequest)
		}
		a.PostPublic = alias.PostPublic
		a.ListMembers = alias.ListMembers
		a.AllowMsgFrom = alias.AllowMsgFrom
		d.Aliases = maps.Clone(d.Aliases)
		d.Aliases[addr.Localpart.String()] = a
		return nil
	})
}

func AliasRemove(ctx context.Context, addr smtp.Address) error {
	return DomainSave(ctx, addr.Domain.Name(), func(d *config.Domain) error {
		_, ok := d.Aliases[addr.Localpart.String()]
		if !ok {
			return fmt.Errorf("%w: alias does not exist", ErrRequest)
		}
		d.Aliases = maps.Clone(d.Aliases)
		delete(d.Aliases, addr.Localpart.String())
		return nil
	})
}

func AliasAddressesAdd(ctx context.Context, addr smtp.Address, addresses []string) error {
	if len(addresses) == 0 {
		return fmt.Errorf("%w: at least one address required", ErrRequest)
	}
	return DomainSave(ctx, addr.Domain.Name(), func(d *config.Domain) error {
		alias, ok := d.Aliases[addr.Localpart.String()]
		if !ok {
			return fmt.Errorf("%w: no such alias", ErrRequest)
		}
		alias.Addresses = append(slices.Clone(alias.Addresses), addresses...)
		alias.ParsedAddresses = nil
		d.Aliases = maps.Clone(d.Aliases)
		d.Aliases[addr.Localpart.String()] = alias
		return nil
	})
}

func AliasAddressesRemove(ctx context.Context, addr smtp.Address, addresses []string) error {
	if len(addresses) == 0 {
		return fmt.Errorf("%w: need at least one address", ErrRequest)
	}
	return DomainSave(ctx, addr.Domain.Name(), func(d *config.Domain) error {
		alias, ok := d.Aliases[addr.Localpart.String()]
		if !ok {
			return fmt.Errorf("%w: no such alias", ErrRequest)
		}
		alias.Addresses = slices.DeleteFunc(slices.Clone(alias.Addresses), func(addr string) bool {
			n := len(addresses)
			addresses = slices.DeleteFunc(addresses, func(a string) bool { return a == addr })
			return n > len(addresses)
		})
		if len(addresses) > 0 {
			return fmt.Errorf("%w: address not found: %s", ErrRequest, strings.Join(addresses, ", "))
		}
		alias.ParsedAddresses = nil
		d.Aliases = maps.Clone(d.Aliases)
		d.Aliases[addr.Localpart.String()] = alias
		return nil
	})
}

// AccountSave updates the configuration of an account. Function xmodify is called
// with a shallow copy of the current configuration of the account. It must not
// change referencing fields (e.g. existing slice/map/pointer), they may still be
// in use, and the change may be rolled back. Referencing values must be copied and
// replaced by the modify. The function may raise a panic for error handling.
func AccountSave(ctx context.Context, account string, xmodify func(acc *config.Account)) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("saving account fields", rerr, slog.String("account", account))
		}
	}()

	defer mox.Conf.DynamicLockUnlock()()

	c := mox.Conf.Dynamic
	acc, ok := c.Accounts[account]
	if !ok {
		return fmt.Errorf("%w: account not present", ErrRequest)
	}

	xmodify(&acc)

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Accounts = map[string]config.Account{}
	for name, a := range c.Accounts {
		nc.Accounts[name] = a
	}
	nc.Accounts[account] = acc

	if err := mox.WriteDynamicLocked(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %w", err)
	}
	log.Info("account fields saved", slog.String("account", account))
	return nil
}

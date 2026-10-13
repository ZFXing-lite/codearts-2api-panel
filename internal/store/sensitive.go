package store

import "strings"

var sensitiveMetadataKeys = []string{"user_token", "access_token", "refresh_token"}

func isEncrypted(value string) bool {
	return strings.HasPrefix(value, "enc:")
}

func (db *DB) sensitiveNeedsMigration(a *Account) bool {
	if a == nil {
		return false
	}
	if (a.Password != "" && !isEncrypted(a.Password)) || (a.RefreshToken != "" && !isEncrypted(a.RefreshToken)) {
		return true
	}
	if a.Credentials != nil {
		c := a.Credentials
		if (c.AccessKeyID != "" && !isEncrypted(c.AccessKeyID)) ||
			(c.SecretAccessKey != "" && !isEncrypted(c.SecretAccessKey)) ||
			(c.SecurityToken != "" && !isEncrypted(c.SecurityToken)) {
			return true
		}
		if c.DpopKeyPair != nil && c.DpopKeyPair.PrivateKeyJwk != "" && !isEncrypted(c.DpopKeyPair.PrivateKeyJwk) {
			return true
		}
	}
	for _, key := range sensitiveMetadataKeys {
		if v := a.Metadata[key]; v != "" && !isEncrypted(v) {
			return true
		}
	}
	return false
}

func (db *DB) decryptSensitive(a *Account) error {
	if a == nil {
		return nil
	}
	var err error
	if a.Password, err = db.fieldCipher.decrypt(a.Password); err != nil {
		return err
	}
	if a.RefreshToken, err = db.fieldCipher.decrypt(a.RefreshToken); err != nil {
		return err
	}
	if a.Credentials != nil {
		c := a.Credentials
		if c.AccessKeyID, err = db.fieldCipher.decrypt(c.AccessKeyID); err != nil {
			return err
		}
		if c.SecretAccessKey, err = db.fieldCipher.decrypt(c.SecretAccessKey); err != nil {
			return err
		}
		if c.SecurityToken, err = db.fieldCipher.decrypt(c.SecurityToken); err != nil {
			return err
		}
		if c.DpopKeyPair != nil {
			if c.DpopKeyPair.PrivateKeyJwk, err = db.fieldCipher.decrypt(c.DpopKeyPair.PrivateKeyJwk); err != nil {
				return err
			}
		}
	}
	for _, key := range sensitiveMetadataKeys {
		if v := a.Metadata[key]; v != "" {
			if a.Metadata[key], err = db.fieldCipher.decrypt(v); err != nil {
				return err
			}
		}
	}
	return nil
}

func (db *DB) encryptSensitive(a *Account) {
	if a == nil {
		return
	}
	a.Password = db.fieldCipher.encrypt(a.Password)
	a.RefreshToken = db.fieldCipher.encrypt(a.RefreshToken)
	if a.Credentials != nil {
		c := a.Credentials
		c.AccessKeyID = db.fieldCipher.encrypt(c.AccessKeyID)
		c.SecretAccessKey = db.fieldCipher.encrypt(c.SecretAccessKey)
		c.SecurityToken = db.fieldCipher.encrypt(c.SecurityToken)
		if c.DpopKeyPair != nil {
			c.DpopKeyPair.PrivateKeyJwk = db.fieldCipher.encrypt(c.DpopKeyPair.PrivateKeyJwk)
		}
	}
	for _, key := range sensitiveMetadataKeys {
		if v := a.Metadata[key]; v != "" {
			a.Metadata[key] = db.fieldCipher.encrypt(v)
		}
	}
}

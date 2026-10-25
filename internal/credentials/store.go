package credentials

import "github.com/zalando/go-keyring"

type Store interface {
	Set(service, account, secret string) error
	Get(service, account string) (string, error)
	Delete(service, account string) error
}

type System struct{}

func (System) Set(service, account, secret string) error {
	return keyring.Set(service, account, secret)
}
func (System) Get(service, account string) (string, error) { return keyring.Get(service, account) }
func (System) Delete(service, account string) error        { return keyring.Delete(service, account) }

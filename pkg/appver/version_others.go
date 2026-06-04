//go:build !darwin && !windows

package appver

func (i *Info) initialize() error {
	return nil
}

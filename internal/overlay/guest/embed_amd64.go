//go:build amd64

package guest

import _ "embed"

//go:embed netinit-linux-amd64.gz
var compressed []byte

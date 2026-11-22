//go:build arm64

package guest

import _ "embed"

//go:embed netinit-linux-arm64.gz
var compressed []byte

package distill

import "os"

func osEnviron() []string { return os.Environ() }

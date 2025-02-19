/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreedto in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cli

// This plugin imports ldapauthserver to register the LDAP implementation of AuthServer.

import (
	"vitess.io/vitess/go/mysql/ldapauthserver"
	"vitess.io/vitess/go/vt/vtgate"
)

func init() {
	vtgate.RegisterPluginInitializer(func() { ldapauthserver.Init() })
}

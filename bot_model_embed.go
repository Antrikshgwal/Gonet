package gonet

import _ "embed"

// BotModel is the trained behavior-cloning network baked into the binary so the
// deployed server's "play vs bot" button has an MLP opponent with no setup.
//
//go:embed bot_model.json
var BotModel []byte

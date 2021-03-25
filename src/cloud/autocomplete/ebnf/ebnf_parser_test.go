package ebnf_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"pixielabs.ai/pixielabs/src/cloud/autocomplete/ebnf"
)

type arg struct {
	Type string
	Name string
}

func TestParseInput(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		expectedAction string
		expectedArgs   []*arg
	}{
		{
			name:           "go command",
			input:          "go profile",
			expectedAction: "go",
			expectedArgs: []*arg{
				{
					Type: "",
					Name: "profile",
				},
			},
		},
		{
			name:           "go command no arg",
			input:          "go ",
			expectedAction: "go",
			expectedArgs:   []*arg{},
		},
		{
			name:           "go command no arg",
			input:          "go ",
			expectedAction: "go",
			expectedArgs:   []*arg{},
		},
		{
			name:           "no action with arg",
			input:          "px/svc",
			expectedAction: "",
			expectedArgs: []*arg{
				{
					Type: "",
					Name: "px/svc",
				},
			},
		},
		{
			name:           "run action with arg",
			input:          "run px/svc",
			expectedAction: "run",
			expectedArgs: []*arg{
				{
					Type: "",
					Name: "px/svc",
				},
			},
		},
		{
			name:           "with args",
			input:          "script:px/svc abcd svc:pl/frontend",
			expectedAction: "",
			expectedArgs: []*arg{
				{
					Type: "script",
					Name: "px/svc",
				},
				{
					Type: "",
					Name: "abcd",
				},
				{
					Type: "svc",
					Name: "pl/frontend",
				},
			},
		},
		{
			name:           "action at end",
			input:          "script:px/svc abcd svc:pl/frontend go",
			expectedAction: "",
			expectedArgs: []*arg{
				{
					Type: "script",
					Name: "px/svc",
				},
				{
					Type: "",
					Name: "abcd",
				},
				{
					Type: "svc",
					Name: "pl/frontend",
				},
				{
					Type: "",
					Name: "go",
				},
			},
		},
		{
			name:           "cursor",
			input:          "script:px-test/svc ab$0cd svc:pl/frontend go",
			expectedAction: "",
			expectedArgs: []*arg{
				{
					Type: "script",
					Name: "px-test/svc",
				},
				{
					Type: "",
					Name: "ab$0cd",
				},
				{
					Type: "svc",
					Name: "pl/frontend",
				},
				{
					Type: "",
					Name: "go",
				},
			},
		},
		{
			name:           "empty value",
			input:          "script:px/svc ab$0cd svc:",
			expectedAction: "",
			expectedArgs: []*arg{
				{
					Type: "script",
					Name: "px/svc",
				},
				{
					Type: "",
					Name: "ab$0cd",
				},
				{
					Type: "svc",
					Name: "",
				},
			},
		},
		{
			name:           "two empty values",
			input:          "script:px/svc ab$0cd svc: svc2:",
			expectedAction: "",
			expectedArgs: []*arg{
				{
					Type: "script",
					Name: "px/svc",
				},
				{
					Type: "",
					Name: "ab$0cd",
				},
				{
					Type: "svc",
					Name: "",
				},
				{
					Type: "svc2",
					Name: "",
				},
			},
		},
		{
			name:           "two values",
			input:          "script:px/svc ab$0cd svc: svc2:test",
			expectedAction: "",
			expectedArgs: []*arg{
				{
					Type: "script",
					Name: "px/svc",
				},
				{
					Type: "",
					Name: "ab$0cd",
				},
				{
					Type: "svc",
					Name: "",
				},
				{
					Type: "svc2",
					Name: "test",
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmd, err := ebnf.ParseInput(test.input)
			assert.Nil(t, err)
			assert.NotNil(t, cmd)
			if test.expectedAction != "" {
				assert.Equal(t, test.expectedAction, *cmd.Action)
			} else {
				assert.Nil(t, cmd.Action)
			}

			if len(test.expectedArgs) == 0 {
				assert.Nil(t, cmd.Args)
			} else {
				assert.Equal(t, len(test.expectedArgs), len(cmd.Args))
				for i, a := range test.expectedArgs {
					if a.Type == "" {
						assert.Nil(t, cmd.Args[i].Type)
					} else {
						assert.Equal(t, a.Type, *cmd.Args[i].Type)
					}

					if a.Name == "" {
						assert.Nil(t, cmd.Args[i].Name)
					} else {
						assert.Equal(t, a.Name, *cmd.Args[i].Name)
					}
				}
			}
		})
	}
}

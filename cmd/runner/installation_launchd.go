package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
)

func parseLaunchdTarget(scope serviceScope, data []byte) (*installationTarget, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := d.Token()
		if err != nil {
			return nil, errors.New("invalid launchd property list")
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local == "dict" {
			value, err := readPlistValue(d, start)
			if err != nil {
				return nil, err
			}
			props, ok := value.(map[string]any)
			if !ok || props["Label"] != launchdLabel {
				return nil, errors.New("unexpected launchd service label")
			}
			raw, ok := props["ProgramArguments"].([]any)
			if !ok || len(raw) == 0 {
				return nil, errors.New("launchd service has no arguments")
			}
			var args []string
			for _, item := range raw {
				arg, ok := item.(string)
				if !ok {
					return nil, errors.New("invalid launchd argument")
				}
				args = append(args, arg)
			}
			target, err := targetFromArgs(scope, args[0], args)
			if err != nil {
				return nil, err
			}
			if user, ok := props["UserName"].(string); ok {
				target.RuntimeUser = user
			}
			if group, ok := props["GroupName"].(string); ok {
				target.RuntimeGroup = group
			}
			if home, ok := props["WorkingDirectory"].(string); ok {
				target.WorkingDirectory = home
			}
			if env, ok := props["EnvironmentVariables"].(map[string]any); ok {
				for k, v := range env {
					if text, ok := v.(string); ok {
						target.Environment[k] = text
					} else {
						return nil, errors.New("invalid launchd environment")
					}
				}
			}
			return target, nil
		}
	}
}

func readPlistValue(d *xml.Decoder, start xml.StartElement) (any, error) {
	switch start.Name.Local {
	case "string", "key", "integer", "real", "date", "data":
		var value string
		if err := d.DecodeElement(&value, &start); err != nil {
			return nil, err
		}
		return value, nil
	case "true", "false":
		if err := d.Skip(); err != nil {
			return nil, err
		}
		return start.Name.Local == "true", nil
	case "dict", "array":
		values := map[string]any{}
		var list []any
		var key string
		for {
			token, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := token.(type) {
			case xml.EndElement:
				if t.Name == start.Name {
					if key != "" {
						return nil, errors.New("property list key has no value")
					}
					if start.Name.Local == "dict" {
						return values, nil
					}
					return list, nil
				}
			case xml.StartElement:
				value, err := readPlistValue(d, t)
				if err != nil {
					return nil, err
				}
				if start.Name.Local == "array" {
					list = append(list, value)
					continue
				}
				if key == "" {
					if t.Name.Local != "key" {
						return nil, errors.New("property list dictionary requires a key")
					}
					key, _ = value.(string)
					if key == "" {
						return nil, errors.New("empty property list key")
					}
					if _, exists := values[key]; exists {
						return nil, errors.New("duplicate property list key")
					}
				} else {
					values[key] = value
					key = ""
				}
			}
		}
	}
	return nil, io.ErrUnexpectedEOF
}

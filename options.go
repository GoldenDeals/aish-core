package aish

import (
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// Option reads one option of a module directive. The Dispenser stands on the
// option's name; the option reads its arguments from the rest of the line.
type Option func(d *caddyfile.Dispenser) error

// StringOption stores the single argument of an option in dst.
func StringOption(dst *string) Option {
	return func(d *caddyfile.Dispenser) error {
		if !d.Args(dst) || len(d.RemainingArgs()) > 0 {
			return d.ArgErr()
		}

		return nil
	}
}

// FlagOption sets dst for an option without arguments.
func FlagOption(dst *bool) Option {
	return func(d *caddyfile.Dispenser) error {
		if len(d.RemainingArgs()) > 0 {
			return d.ArgErr()
		}
		*dst = true

		return nil
	}
}

// ReadOptions reads what follows the module name in a directive, for
// UnmarshalCaddyfile. The options can stand on the directive's line, in the
// block, or in both:
//
//	llm anthropic model claude-x max_tokens 64000
//	llm anthropic model claude-x {
//	    max_tokens 64000
//	}
//
// Leading arguments that are not option names are returned as positional
// arguments. On the line, an option's arguments are the tokens up to the next
// option name, so an option with a variable number of arguments goes last, and
// an argument cannot be spelled like an option name. The Dispenser must stand
// on the module name.
func ReadOptions(d *caddyfile.Dispenser, options map[string]Option) ([]string, error) {
	file, line := d.File(), d.Line()
	args := d.RemainingArgs()

	var positional []string
	i := 0
	for i < len(args) && options[args[i]] == nil {
		positional = append(positional, args[i])
		i++
	}
	for i < len(args) {
		j := i + 1
		for j < len(args) && options[args[j]] == nil {
			j++
		}
		tokens := make([]caddyfile.Token, 0, j-i)
		for _, text := range args[i:j] {
			tokens = append(tokens, caddyfile.Token{File: file, Line: line, Text: text})
		}
		sub := caddyfile.NewDispenser(tokens)
		sub.Next()
		if err := options[args[i]](sub); err != nil {
			return nil, err
		}
		i = j
	}

	for d.NextBlock(0) {
		option := options[d.Val()]
		if option == nil {
			return nil, d.Errf("unknown option %q", d.Val())
		}
		if err := option(d); err != nil {
			return nil, err
		}
	}

	return positional, nil
}

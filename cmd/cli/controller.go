package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/xheize/git-updater/internal/controller"
	"github.com/xheize/git-updater/internal/gitManager"
)

type changeFlags []string

func (v *changeFlags) String() string     { return strings.Join(*v, ",") }
func (v *changeFlags) Set(s string) error { *v = append(*v, s); return nil }

func runControllerCLI(args []string, out io.Writer) error {
	command := args[0]
	if command == "help" {
		fmt.Fprintln(out, "GitOps Change Controller\nCommands: inspect, images, plan, apply, show, changesets, jobs, job, retry\nExample: plan --change ghcr.io/foo/api=v2 --change ghcr.io/foo/worker=v3\nThen: apply --id <reviewed-plan-id>\nUse <command> --help for flags. No web UI is provided.")
		return nil
	}
	switch command {
	case "inspect", "images", "plan", "apply", "show", "changesets", "jobs", "job", "retry":
	default:
		return fmt.Errorf("unknown command %q; use help", command)
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(out)
	server := fs.String("server", os.Getenv("GIT_UPDATER_SERVER_URL"), "Controller URL (default http://localhost:3000)")
	key := fs.String("key", os.Getenv("GIT_UPDATER_API_KEY"), "API key (prefer GIT_UPDATER_API_KEY environment variable)")
	jsonOutput := fs.Bool("json", false, "Print machine-readable JSON")
	id := fs.String("id", "", "Plan/job/intent ID")
	image := fs.String("image", "", "Image name filter or single change image")
	tag := fs.String("tag", "", "Single change tag")
	file := fs.String("file", "", "Restrict mutation to a source or override YAML file")
	envs := fs.String("env", "", "Comma-separated environment IDs from inspect/images")
	intentFile := fs.String("intent", "", "Read a multi-image Intent JSON file")
	limit := fs.Int("limit", 20, "History page size (1-100)")
	offset := fs.Int("offset", 0, "History offset")
	var changes changeFlags
	fs.Var(&changes, "change", "Repeatable image=tag change")
	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *server == "" {
		*server = "http://localhost:3000"
	}
	if !strings.Contains(*server, "://") {
		*server = "http://" + *server
	}
	*server = strings.TrimRight(*server, "/")
	if *key == "" {
		*key = os.Getenv("API_KEY")
	}
	if *key == "" {
		return fmt.Errorf("GIT_UPDATER_API_KEY is required")
	}
	method, path, body := "GET", "", []byte(nil)
	switch command {
	case "inspect", "images":
		path = "/api/repository"
	case "changesets", "jobs":
		path = "/api/" + command + "?limit=" + strconv.Itoa(*limit) + "&offset=" + strconv.Itoa(*offset)
	case "show", "apply", "job", "retry":
		if *id == "" {
			return fmt.Errorf("--id is required")
		}
		group := "changesets"
		if command == "job" || command == "retry" {
			group = "jobs"
		}
		path = "/api/" + group + "/" + url.PathEscape(*id)
		if command == "apply" || command == "retry" {
			method = "POST"
			path += "/" + command
		}
	case "plan":
		in := controller.Intent{ID: *id, Changes: []controller.Change{}}
		if *intentFile != "" {
			if len(changes) > 0 || *image != "" || *tag != "" || *file != "" || *envs != "" {
				return fmt.Errorf("--intent cannot be combined with change selection flags")
			}
			data, err := os.ReadFile(*intentFile)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(data, &in); err != nil {
				return err
			}
			if *id != "" {
				in.ID = *id
			}
		} else {
			if *image != "" || *tag != "" {
				if *image == "" || *tag == "" {
					return fmt.Errorf("--image and --tag must be supplied together")
				}
				changes = append(changes, *image+"="+*tag)
			}
			for _, entry := range changes {
				name, tag, ok := strings.Cut(entry, "=")
				if !ok {
					return fmt.Errorf("--change must be image=tag")
				}
				c := controller.Change{Image: name, Tag: tag, File: *file}
				for _, env := range strings.Split(*envs, ",") {
					if e := strings.TrimSpace(env); e != "" {
						c.Environments = append(c.Environments, e)
					}
				}
				in.Changes = append(in.Changes, c)
			}
		}
		if in.ID == "" {
			var token [16]byte
			if _, err := rand.Read(token[:]); err != nil {
				return err
			}
			in.ID = "cli-" + hex.EncodeToString(token[:])
		}
		if err := controller.ValidateIntent(in); err != nil {
			return err
		}
		var err error
		body, err = json.Marshal(in)
		if err != nil {
			return err
		}
		method, path = "POST", "/api/changesets"
	}
	req, err := http.NewRequest(method, *server+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+*key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 4 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w; for an uncertain apply, inspect the same plan ID before sending a new intent", err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 128<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(data)))
	}
	if command == "images" && *image != "" {
		var v gitManager.RepositoryView
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		uses := []controller.Use{}
		for _, u := range v.Model.Uses {
			if controller.ImageName(u.SourceImage) == controller.ImageName(*image) || controller.ImageName(u.EffectiveImage) == controller.ImageName(*image) {
				uses = append(uses, u)
			}
		}
		v.Model.Uses = uses
		data, err = json.Marshal(v)
		if err != nil {
			return err
		}
	}
	if *jsonOutput {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, data, "", "  "); err != nil {
			return err
		}
		fmt.Fprintln(out, pretty.String())
		return nil
	}
	switch command {
	case "inspect", "images":
		var v gitManager.RepositoryView
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		fmt.Fprintf(out, "Repository: %s / %s\nBranch: %s\nRevision: %s\nVerified: %t  Writable: %t\n%s\nFiles: %d  Resources: %d  Image uses: %d\n", v.Identity.Provider, v.Identity.Name, v.Branch, v.Model.Revision, v.Identity.Verified, v.Identity.Writable, v.Identity.Detail, len(v.Model.Files), len(v.Model.Resources), len(v.Model.Uses))
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ENVIRONMENT\tRESOURCE\tSOURCE\tEFFECTIVE\tMUTATION TARGET")
		for _, u := range v.Model.Uses {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s:%d\n", u.Environment, u.Resource, u.SourceImage, u.EffectiveImage, u.Target.File, u.Target.Line)
		}
		w.Flush()
		for _, d := range v.Model.Diagnostics {
			fmt.Fprintf(out, "BLOCKED %s [%s]: %s\n", d.File, d.Code, d.Message)
		}
	case "plan", "show", "apply":
		var p controller.Plan
		if err := json.Unmarshal(data, &p); err != nil {
			return err
		}
		fmt.Fprintf(out, "ChangeSet: %s\nState: %s\nBase: %s (%s)\nAtomic: %t  Mutations: %d  Affected uses: %d\n", p.ID, p.State, p.BaseRevision, p.Branch, p.Atomic, len(p.Mutations), len(p.Impact))
		for _, a := range p.Artifacts {
			fmt.Fprintf(out, "Artifact: %s:%s -> %s\n", a.Image, a.Tag, a.Digest)
		}
		for _, i := range p.Impact {
			fmt.Fprintf(out, "%s / %s: %s -> %s\n", i.Environment, i.Resource, i.Current, i.Proposed)
		}
		for _, d := range p.Diffs {
			fmt.Fprint(out, d.Diff)
		}
		if p.Commit != "" {
			fmt.Fprintln(out, "Commit:", p.Commit)
		}
		if p.State == "planned" {
			fmt.Fprintf(out, "Preview only. Review, then run: apply --id %s\n", p.ID)
		}
		if p.Error != "" {
			fmt.Fprintln(out, p.Error)
		}
	default:
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, data, "", "  "); err != nil {
			return err
		}
		fmt.Fprintln(out, pretty.String())
	}
	return nil
}

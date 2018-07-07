package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/containers/image/copy"
	"github.com/containers/image/docker/reference"
	"github.com/containers/image/manifest"
	"github.com/containers/image/transports"
	"github.com/containers/image/transports/alltransports"
	"github.com/containers/image/types"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/urfave/cli"
)

// contextsFromGlobalOptions returns source and destionation types.SystemContext depending on c.
func contextsFromGlobalOptions(c *cli.Context) (*types.SystemContext, *types.SystemContext, error) {
	sourceCtx, err := contextFromGlobalOptions(c, "src-")
	if err != nil {
		return nil, nil, err
	}

	destinationCtx, err := contextFromGlobalOptions(c, "dest-")
	if err != nil {
		return nil, nil, err
	}

	return sourceCtx, destinationCtx, nil
}

type copyOptions struct {
	additionalTags    cli.StringSlice // For docker-archive: destinations, in addition to the name:tag specified as destination, also add these
	removeSignatures  bool            // Do not copy signatures from the source image
	signByFingerprint string          // Sign the image using a GPG key with the specified fingerprint
	format            optionalString  // Force conversion of the image to a specified format
}

func copyCmd() cli.Command {
	opts := copyOptions{}
	return cli.Command{
		Name:  "copy",
		Usage: "Copy an IMAGE-NAME from one location to another",
		Description: fmt.Sprintf(`

	Container "IMAGE-NAME" uses a "transport":"details" format.

	Supported transports:
	%s

	See skopeo(1) section "IMAGE NAMES" for the expected format
	`, strings.Join(transports.ListNames(), ", ")),
		ArgsUsage: "SOURCE-IMAGE DESTINATION-IMAGE",
		Action:    opts.run,
		// FIXME: Do we need to namespace the GPG aspect?
		Flags: []cli.Flag{
			cli.StringSliceFlag{
				Name:  "additional-tag",
				Usage: "additional tags (supports docker-archive)",
				Value: &opts.additionalTags, // Surprisingly StringSliceFlag does not support Destination:, but modifies Value: in place.
			},
			cli.StringFlag{
				Name:  "authfile",
				Usage: "path of the authentication file. Default is ${XDG_RUNTIME_DIR}/containers/auth.json",
			},
			cli.BoolFlag{
				Name:        "remove-signatures",
				Usage:       "Do not copy signatures from SOURCE-IMAGE",
				Destination: &opts.removeSignatures,
			},
			cli.StringFlag{
				Name:        "sign-by",
				Usage:       "Sign the image using a GPG key with the specified `FINGERPRINT`",
				Destination: &opts.signByFingerprint,
			},
			cli.StringFlag{
				Name:  "src-creds, screds",
				Value: "",
				Usage: "Use `USERNAME[:PASSWORD]` for accessing the source registry",
			},
			cli.StringFlag{
				Name:  "dest-creds, dcreds",
				Value: "",
				Usage: "Use `USERNAME[:PASSWORD]` for accessing the destination registry",
			},
			cli.StringFlag{
				Name:  "src-cert-dir",
				Value: "",
				Usage: "use certificates at `PATH` (*.crt, *.cert, *.key) to connect to the source registry or daemon",
			},
			cli.BoolTFlag{
				Name:  "src-tls-verify",
				Usage: "require HTTPS and verify certificates when talking to the container source registry or daemon (defaults to true)",
			},
			cli.StringFlag{
				Name:  "dest-cert-dir",
				Value: "",
				Usage: "use certificates at `PATH` (*.crt, *.cert, *.key) to connect to the destination registry or daemon",
			},
			cli.BoolTFlag{
				Name:  "dest-tls-verify",
				Usage: "require HTTPS and verify certificates when talking to the container destination registry or daemon (defaults to true)",
			},
			cli.StringFlag{
				Name:  "dest-ostree-tmp-dir",
				Value: "",
				Usage: "`DIRECTORY` to use for OSTree temporary files",
			},
			cli.StringFlag{
				Name:  "src-shared-blob-dir",
				Value: "",
				Usage: "`DIRECTORY` to use to fetch retrieved blobs (OCI layout sources only)",
			},
			cli.StringFlag{
				Name:  "dest-shared-blob-dir",
				Value: "",
				Usage: "`DIRECTORY` to use to store retrieved blobs (OCI layout destinations only)",
			},
			cli.GenericFlag{
				Name:  "format, f",
				Usage: "`MANIFEST TYPE` (oci, v2s1, or v2s2) to use when saving image to directory using the 'dir:' transport (default is manifest type of source)",
				Value: newOptionalStringValue(&opts.format),
			},
			cli.BoolFlag{
				Name:  "dest-compress",
				Usage: "Compress tarball image layers when saving to directory using the 'dir' transport. (default is same compression type as source)",
			},
			cli.StringFlag{
				Name:  "src-daemon-host",
				Value: "",
				Usage: "use docker daemon host at `HOST` (docker-daemon sources only)",
			},
			cli.StringFlag{
				Name:  "dest-daemon-host",
				Value: "",
				Usage: "use docker daemon host at `HOST` (docker-daemon destinations only)",
			},
		},
	}
}

func (opts *copyOptions) run(c *cli.Context) error {
	if len(c.Args()) != 2 {
		cli.ShowCommandHelp(c, "copy")
		return errors.New("Exactly two arguments expected")
	}

	policyContext, err := getPolicyContext(c)
	if err != nil {
		return fmt.Errorf("Error loading trust policy: %v", err)
	}
	defer policyContext.Destroy()

	srcRef, err := alltransports.ParseImageName(c.Args()[0])
	if err != nil {
		return fmt.Errorf("Invalid source name %s: %v", c.Args()[0], err)
	}
	destRef, err := alltransports.ParseImageName(c.Args()[1])
	if err != nil {
		return fmt.Errorf("Invalid destination name %s: %v", c.Args()[1], err)
	}

	sourceCtx, destinationCtx, err := contextsFromGlobalOptions(c)
	if err != nil {
		return err
	}

	var manifestType string
	if opts.format.present {
		switch opts.format.value {
		case "oci":
			manifestType = imgspecv1.MediaTypeImageManifest
		case "v2s1":
			manifestType = manifest.DockerV2Schema1SignedMediaType
		case "v2s2":
			manifestType = manifest.DockerV2Schema2MediaType
		default:
			return fmt.Errorf("unknown format %q. Choose one of the supported formats: 'oci', 'v2s1', or 'v2s2'", opts.format.value)
		}
	}

	for _, image := range opts.additionalTags {
		ref, err := reference.ParseNormalizedNamed(image)
		if err != nil {
			return fmt.Errorf("error parsing additional-tag '%s': %v", image, err)
		}
		namedTagged, isNamedTagged := ref.(reference.NamedTagged)
		if !isNamedTagged {
			return fmt.Errorf("additional-tag '%s' must be a tagged reference", image)
		}
		destinationCtx.DockerArchiveAdditionalTags = append(destinationCtx.DockerArchiveAdditionalTags, namedTagged)
	}

	ctx, cancel := commandTimeoutContextFromGlobalOptions(c)
	defer cancel()

	_, err = copy.Image(ctx, policyContext, destRef, srcRef, &copy.Options{
		RemoveSignatures:      opts.removeSignatures,
		SignBy:                opts.signByFingerprint,
		ReportWriter:          os.Stdout,
		SourceCtx:             sourceCtx,
		DestinationCtx:        destinationCtx,
		ForceManifestMIMEType: manifestType,
	})
	return err
}

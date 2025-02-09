package main

import (
	"fmt"
	_url "net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/adohkan/git-remote-https-iap/internal/git"
	"github.com/adohkan/git-remote-https-iap/internal/iap"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

const (
	// DebugEnvVariable is the name of the environment variable that needs to be set in order to enable debug logging
	DebugEnvVariable = "GIT_IAP_VERBOSE"
	DebugEnv         = "DEBUG"
)

var (
	binaryName = os.Args[0]
	version    string

	// only used in configureCmd
	repoURL, helperID, helperSecret, clientID string
	helperName                                string

	// Only used in checkcmd
	forcebrowser bool

	rootCmd = &cobra.Command{
		Use:   fmt.Sprintf("%s remote url", binaryName),
		Short: "git-remote-helper that handles authentication for GCP Identity Aware Proxy",
		Args:  cobra.ExactArgs(2),
		Run:   execute,
	}

	versionCmd = &cobra.Command{
		Use:   "version",
		Short: "Print version number",
		Run:   printVersion,
	}

	installProtocolCmd = &cobra.Command{
		Use:   "install",
		Short: "Install protocol in Git config",
		Run:   installGitProtocol,
	}

	configureCmd = &cobra.Command{
		Use:   "configure",
		Short: "Configure IAP for a given repository",
		Run:   configureIAP,
	}

	checkCmd = &cobra.Command{
		Use:   "check [url]",
		Short: "Refresh token for remote url if needed, then exit",
		Run:   check,
	}

	printCmd = &cobra.Command{
		Use:   "print [url]",
		Short: "Refresh token for remote url if needed, then print to stdout",
		Run:   print,
	}
)

func init() {
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(installProtocolCmd)
	rootCmd.AddCommand(checkCmd)
	rootCmd.AddCommand(printCmd)

	configureCmd.Flags().StringVar(&repoURL, "repoURL", "", "URL of the git repository to configure (required)")
	configureCmd.MarkFlagRequired("repoURL")
	configureCmd.Flags().StringVar(&helperID, "helperID", "", "OAuth Client ID for the helper (required)")
	configureCmd.MarkFlagRequired("helperID")
	configureCmd.Flags().StringVar(&helperSecret, "helperSecret", "", "OAuth Client Secret for the helper (required)")
	configureCmd.MarkFlagRequired("helperSecret")
	configureCmd.Flags().StringVar(&clientID, "clientID", "", "OAuth Client ID of the IAP instance (required)")
	configureCmd.MarkFlagRequired("clientID")
	configureCmd.Flags().StringVar(&helperName, "helperName", "https+iap", "Name of the gitremote-helper, for example \"iap\" if PATH has a git-remote-iap binary")

	checkCmd.Flags().BoolVarP(&forcebrowser, "forcebrowser", "f", false, "Forces browser refresh flow")

	rootCmd.AddCommand(configureCmd)

	// set log level
	zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	if debug, _ := strconv.ParseBool(os.Getenv(DebugEnvVariable)); debug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}
	if debug, _ := strconv.ParseBool(os.Getenv(DebugEnv)); debug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func execute(cmd *cobra.Command, args []string) {
	remote, url := args[0], args[1]
	log.Debug().Msgf("%s %s %s", binaryName, remote, url)

	c := handleIAPAuthCookieFor(url, false)
	git.PassThruRemoteHTTPSHelper(remote, url, c.Cookie.Token.Raw)
}

func check(cmd *cobra.Command, args []string) {
	remote, url := args[0], args[len(args)-1]
	log.Debug().Msgf("%s check %s %s: forcebrowser=%s", binaryName, remote, url, strconv.FormatBool(forcebrowser))

	handleIAPAuthCookieFor(url, forcebrowser)
}

func print(cmd *cobra.Command, args []string) {
	url := args[0]
	log.Debug().Msgf("%s print %s", binaryName, url)

	auth := handleIAPAuthCookieFor(url, false)
	fmt.Printf("%s\n", auth.RawToken)
}

func printVersion(cmd *cobra.Command, args []string) {
	fmt.Printf("%s %s\n", binaryName, version)
}

func installGitProtocol(cmd *cobra.Command, args []string) {
	p := strings.TrimLeft(binaryName, "git-remote-")
	git.InstallProtocol(p)
	log.Info().Msgf("%s protocol configured in git!", p)
}

func configureIAP(cmd *cobra.Command, args []string) {
	repo, err := _url.Parse(repoURL)
	https := fmt.Sprintf("https://%s", repo.Host)
	if err != nil {
		log.Error().Msgf("Could not convert %s in https://: %s", https, err)
	}

	log.Info().Msgf("Configure IAP for %s", https)
	git.SetGlobalConfig(https, "iap", "helperID", helperID)
	git.SetGlobalConfig(https, "iap", "helperSecret", helperSecret)
	git.SetGlobalConfig(https, "iap", "clientID", clientID)

	// let users manipulate standard 'https://' urls
	insteadOf := &git.GitConfig{
		Url:     fmt.Sprintf("%s://%s", helperName, repo.Host),
		Section: "url",
		Key:     "insteadOf",
		Value:   https,
	}
	if strings.Contains(repo.Host, "*") {
		log.Warn().Msg("While config is valid for wildcard hosts, transparent support for https:// remotes require \"insteadOf\" config")
		log.Info().Msg("Actual hosts must be manually configured as follows (with * replaced by subdomain):")
		log.Info().Msg(insteadOf.CommandSuggestGlobal())
	} else {
		git.SetConfigGlobal(insteadOf)
	}

	// set cookie path
	domainSlug := strings.ReplaceAll(repo.Host, ".", "-")
	domainSlug = strings.ReplaceAll(domainSlug, "*", "_wildcard_")
	cookiePath := fmt.Sprintf("~/.config/gcp-iap/%s.cookie", domainSlug)
	git.SetGlobalConfig(https, "http", "cookieFile", cookiePath)
}

func handleIAPAuthCookieFor(url string, forcebrowserflow bool) *iap.AuthState {
	// All our work will be based on the basedomain of the provided URL
	// as IAP would be setup for the whole domain.
	url, err := toHTTPSBaseDomain(url)
	if err != nil {
		log.Error().Msgf("[handleIAPAuthCookieFor] Could not convert %s in https://: %s", url, err)
	}

	log.Debug().Msgf("[handleIAPAuthCookieFor] Manage IAP auth for %s", url)

	auth, err := iap.ReadAuthState(url)
	switch {
	case err != nil:
		log.Debug().Msgf("[handleIAPAuthCookieFor] Could not read IAP cookie for %s: %s", url, err.Error())
		auth, err = iap.NewAuth(url, forcebrowserflow)
		if err != nil {
			log.Debug().Msgf("[handleIAPAuthCookieFor] Retrying with forcebrowserflow: true")
			auth, err = iap.NewAuth(url, true)
		}
	case auth.Cookie.Expired():
		log.Debug().Msgf("[handleIAPAuthCookieFor] IAP cookie for %s has expired", url)
		auth, err = iap.NewAuth(url, forcebrowserflow)
		if err != nil {
			log.Debug().Msgf("[handleIAPAuthCookieFor] Retrying with forcebrowserflow: true")
			auth, err = iap.NewAuth(url, true)
		}
	case !auth.Cookie.Expired():
		log.Debug().Msgf("[handleIAPAuthCookieFor] IAP Cookie still valid until %s", time.Unix(auth.Cookie.Claims.ExpiresAt, 0))
	}

	if err != nil {
		log.Fatal().Msg(err.Error())
	}

	return auth
}

func toHTTPSBaseDomain(addr string) (string, error) {
	u, err := _url.Parse(addr)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://%s", u.Host), nil
}

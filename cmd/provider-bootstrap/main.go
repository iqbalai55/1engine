package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"billing-backend/internal/identity"
	"billing-backend/internal/store"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "provider-admin" {
		fmt.Fprintln(os.Stderr, "usage: provider-bootstrap provider-admin --email verified@example.com")
		os.Exit(2)
	}
	flags := flag.NewFlagSet("provider-admin", flag.ExitOnError)
	email := flags.String("email", "", "verified portal account email")
	_ = flags.Parse(os.Args[2:])
	if *email == "" {
		fmt.Fprintln(os.Stderr, "--email is required")
		os.Exit(2)
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(1)
	}
	db, err := store.Open(context.Background(), databaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := identity.BootstrapProvider(context.Background(), db.Pool, *email); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("provider admin bootstrapped")
}

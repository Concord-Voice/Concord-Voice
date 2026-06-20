.PHONY: help build run test migrate-up migrate-down migrate-create migrate-version clean

# Default target
help:
	@echo "Concord Control Plane - Available Commands:"
	@echo ""
	@echo "  make build            - Build the control-plane binary"
	@echo "  make run              - Run the control-plane server"
	@echo "  make test             - Run tests"
	@echo "  make migrate-up       - Apply all pending migrations"
	@echo "  make migrate-down     - Rollback last migration"
	@echo "  make migrate-create   - Create new migration (requires NAME=migration_name)"
	@echo "  make migrate-version  - Show current migration version"
	@echo "  make clean            - Remove built binaries"
	@echo ""

# Build the server binary
build:
	@echo "Building control-plane..."
	@go build -o bin/control-plane ./cmd/server

# Build the migrate CLI tool
build-migrate:
	@echo "Building migrate tool..."
	@go build -o bin/migrate ./cmd/migrate

# Run the server
run: build
	@echo "Starting control-plane server..."
	@./bin/control-plane

# Run tests
test:
	@echo "Running tests..."
	@go test -v ./...

# Apply all pending migrations
migrate-up: build-migrate
	@echo "Applying migrations..."
	@./bin/migrate -command=up

# Rollback last migration
migrate-down: build-migrate
	@echo "Rolling back migration..."
	@./bin/migrate -command=down -steps=1

# Create new migration (usage: make migrate-create NAME=add_user_status)
migrate-create: build-migrate
ifndef NAME
	@echo "Error: NAME is required. Usage: make migrate-create NAME=migration_name"
	@exit 1
endif
	@echo "Creating migration: $(NAME)"
	@./bin/migrate -command=create -name=$(NAME)

# Show current migration version
migrate-version: build-migrate
	@echo "Checking migration version..."
	@./bin/migrate -command=version

# Clean built binaries
clean:
	@echo "Cleaning build artifacts..."
	@rm -rf bin/

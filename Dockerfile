# ── Stage 1: Build ───────────────────────────────────────────────────────────
# We use a multi-stage build. The first stage compiles the Go binary using the
# full Go toolchain image. The second stage copies ONLY the compiled binary into
# a minimal scratch/distroless image.
#
# Why multi-stage?
# The full Go image (golang:1.22) is ~800MB. The final image just needs the
# compiled binary — no compiler, no source, no build tools. Result: a ~15MB
# final image instead of ~800MB. Smaller images = faster deploys on Railway,
# smaller attack surface, faster pulls.

FROM golang:1.22-alpine AS builder

# Install git so `go mod download` can fetch modules from VCS if needed.
RUN apk add --no-cache git

WORKDIR /app

# Copy go.mod first and download dependencies BEFORE copying source.
# Docker caches each layer. If only your source code changes (not go.mod),
# Docker reuses the cached `go mod download` layer — much faster rebuilds.
COPY go.mod go.sum* ./
RUN go mod download

# Now copy source and build. This layer busts only when source changes.
COPY . .

# CGO_ENABLED=0: disable C bindings → fully static binary (no libc dependency)
# GOOS=linux: target OS (important if you're building on macOS)
# -ldflags="-w -s": strip debug info and symbol table → smaller binary
# Output binary to /app/logsense
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s" \
    -o /app/logsense \
    ./cmd/server

# ── Stage 2: Runtime ─────────────────────────────────────────────────────────
# alpine is minimal (~5MB) but includes a shell and basic utilities,
# which is useful for debugging on Railway. Use "FROM scratch" for maximum
# minimalism (no shell at all) once you're confident the app is stable.

FROM alpine:3.19

# Add CA certificates — required for HTTPS calls to OpenAI API.
# The scratch base has none; alpine has them via this package.
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

# Copy only the compiled binary from the builder stage.
# Nothing else from the build environment makes it into the final image.
COPY --from=builder /app/logsense .

# Document which port the app listens on (informational — doesn't publish it).
EXPOSE 8080

# Run as a non-root user — security best practice.
# If someone exploits the app, they get a low-privilege user, not root.
RUN addgroup -S logsense && adduser -S logsense -G logsense
USER logsense

CMD ["./logsense"]
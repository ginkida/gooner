# Security Policy

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| latest  | :white_check_mark: |

## Reporting a Vulnerability

If you discover a security vulnerability in Gokin, please report it responsibly.

### How to Report

1. **Do NOT** create a public GitHub issue for security vulnerabilities
2. Send an email to: **ya.ginkida@yandex.kz**
3. Include:
   - Description of the vulnerability
   - Steps to reproduce
   - Potential impact
   - Any suggested fixes (optional)

### What to Expect

- **Acknowledgment**: Within 48 hours
- **Initial Assessment**: Within 7 days
- **Resolution Timeline**: Depends on severity, typically 30-90 days

### Severity Levels

| Level | Description | Response Time |
|-------|-------------|---------------|
| Critical | Remote code execution, credential theft | 24-48 hours |
| High | Privilege escalation, data exposure | 7 days |
| Medium | Limited impact vulnerabilities | 30 days |
| Low | Minor issues | 90 days |

## Security Best Practices

### API Key Management

**Recommended:** Use environment variables instead of config files.

```bash
# Set API key in environment (recommended)
export GEMINI_API_KEY="your-api-key"

# Or use Gokin-specific variable
export GOKIN_GEMINI_KEY="your-api-key"
```

**Avoid:** Storing API keys in `config.yaml`. If you must, ensure the file has restricted permissions:

```bash
chmod 600 ~/.config/gokin/config.yaml
```

### File Permissions

Gokin automatically sets secure permissions:
- Config files: `0600` (owner read/write only)
- Config directory: `0700` (owner only)
- OAuth tokens: `0600` (owner read/write only)

### Blocked Operations

Gokin blocks dangerous commands by default:
- Fork bombs
- Destructive operations (`rm -rf /`, `mkfs`)
- Reverse shells
- Sensitive file access (`.ssh`, `.aws`, `.kube`)

### Path Traversal Protection

- All file paths are validated against the working directory
- Symlinks outside the project are blocked by default
- Parent directory traversal (`../`) is prevented

## Security Features

### Secret Redaction

Gokin automatically redacts sensitive information from logs and outputs:
- API keys (Google, AWS, GitHub, Stripe)
- Bearer/JWT tokens
- Database connection strings
- SSH private keys
- Webhook URLs

### Environment Isolation

Bash commands run with a sanitized environment:
- Only safe environment variables are passed
- API keys and secrets are excluded from subprocesses

### Command Validation

All bash commands are validated against a blocklist before execution.

## Known Limitations

1. **Local execution**: Gokin runs with user permissions
2. **AI-generated code**: Review all AI-suggested code before execution
3. **Third-party APIs**: API keys are sent to Google Gemini servers

## Security Audit

Last security review: January 2025

For questions about security, contact: **ya.ginkida@yandex.kz**

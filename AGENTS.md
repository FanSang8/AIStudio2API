# Deployment

- Validate changes locally, then commit and push to the user's repository:
  `git@github.com:FanSang8/AIStudio2API.git`.
- Stop after pushing. Deploy only when the user explicitly requests deployment
  in a later instruction.
- When deployment is authorized, fetch that repository on the server and build
  the pushed commit. Do not deploy source files by SCP or bypass the repository.
- Preserve `/opt/aistudio2api/data`, account sessions, administrator credentials,
  and the existing Nginx configuration.
- Never commit R2 credentials, local runtime data, or `.env` files.
- The user's R2 bucket has a custom domain. Return images using the configured
  `R2_PUBLIC_BASE_URL`, never the S3 API endpoint or a default r2.dev domain.

# velka

Run commands in multiple project folders in parallel.

## Install

```sh
task install
```

## Usage

```sh
velka
velka -c config.yaml
velka -only api,web
```

## Config

```yaml
vars:
  BRANCH: main
env:
  NODE_ENV: production
projects:
  - name: api
    path: ./services/api
    cmds:
      - git pull origin ${{ .BRANCH }}
      - go build ./...
  - path: ~/code/web
    deps: [api]
    cmds:
      - npm install
      - npm run build
```

- `path`: folder to run in
- `cmds`: run in order, stops on first failure
- `name`: optional, defaults to folder name
- `deps`: wait for these projects to succeed first
- `vars`: used as `${{ .NAME }}` in path, env and cmds
- `env`: environment variables for the commands

`vars` and `env` can be set globally or per project.

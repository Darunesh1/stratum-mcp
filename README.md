# Stratum MCP Server (Standalone)

Stratum MCP is a standalone [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) server built in Go. It enables AI assistants (like Claude Desktop, Gemini, Cursor) to manage configurations, search and retrieve bibliographic data from OpenAlex, compile local DuckDB databases, and run multi-pronged metadata imputation pipelines.

---

## Features

- **Bibliographic Ingest**: Retrieve scholarly metadata from OpenAlex concurrently with built-in rate-limiting logic.
- **SQL Analytics Engine**: Direct read-only SQL querying on local DuckDB files.
- **Metadata Imputation**: Fill in missing author institutional affiliations and countries using Crossref, Gemini LLM classification, and local PDF manuscript text parsing.
- **Stdio Transport**: Seamlessly runs over stdio, compliant with the MCP spec.

---

## Installation & Running

### A. Run via NPX (Recommended)
You can launch the server instantly using `npx` (which resolves the appropriate Go binary for your platform and architecture automatically):

```bash
npx stratum-mcp
```

To configure with your environment variables (like Gemini API key for LLM-based imputations):
```bash
GEMINI_API_KEY="your-api-key" npx stratum-mcp
```

### B. Compile from Source
If you prefer to build the native executable locally:

```bash
# Clone the repository and navigate inside
cd stratum-mcp

# Build the host platform binary
bash scripts/build-binaries.sh

# Run the compiled binary (stored in bin/)
./bin/stratum-mcp-darwin-arm64 # (or corresponding platform binary)
```

---

## Claude Desktop Configuration

To integrate Stratum MCP with Claude Desktop, add the server to your `claude_desktop_config.json` configuration file:

```json
{
  "mcpServers": {
    "stratum-mcp": {
      "command": "npx",
      "args": ["-y", "stratum-mcp"],
      "env": {
        "GEMINI_API_KEY": "YOUR_GEMINI_API_KEY"
      }
    }
  }
}
```

*Note: Make sure to replace `YOUR_GEMINI_API_KEY` with your actual Gemini API key if you plan to use LLM imputation tools.*

---

## Exposed MCP Tools

The server registers the following five tools:

### 1. `validate`
Validates the search keywords syntax and checks if configured topics exist in OpenAlex.
- **Arguments:**
  - `ConfigPath` (string, optional): Path to the database configuration (defaults to `data/db/config.db`).

### 2. `search`
Queries OpenAlex to get the total count of academic papers matching current configuration filters.
- **Arguments:**
  - `ConfigPath` (string, optional): Path to the database configuration.

### 3. `download`
Downloads papers matching the filters concurrently and saves them to a local JSONL file.
- **Arguments:**
  - `ConfigPath` (string, optional): Path to the database configuration.
  - `OutputPath` (string, required): Destination path for the downloaded `.jsonl` file.

### 4. `convert_db`
Parses raw downloaded JSONL paper records and inserts them into DuckDB, initializing the relational tables.
- **Arguments:**
  - `ConfigPath` (string, optional): Path to the database configuration.
  - `JSONLPath` (string, required): Path to the input JSONL file.
  - `DBPath` (string, required): Destination path for the `.db` DuckDB file.

### 5. `impute`
Triggers the multi-pronged imputation pipeline (Crossref DOI lookups, LLMs, and local PDF parsing) to fill in missing metadata.
- **Arguments:**
  - `ConfigPath` (string, optional): Path to the database configuration.
  - `DBPath` (string, required): Path to the DuckDB database.
  - `UseLLM` (boolean, optional): Enable Gemini LLM string classification (requires `GEMINI_API_KEY`).
  - `PDFDir` (string, optional): Directory containing local PDF manuscripts.

---

## License

This project is licensed under the MIT License - see the LICENSE file for details.
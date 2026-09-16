package codeintelbridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/codeintelcontract"
)

type extensionDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func extensionSource() []byte {
	descriptors := codeintelcontract.Descriptors()
	view := make([]extensionDescriptor, len(descriptors))
	for i, descriptor := range descriptors {
		view[i] = extensionDescriptor{descriptor.Name, descriptor.Description, descriptor.InputSchema}
	}
	raw, _ := json.Marshal(view)
	return []byte(fmt.Sprintf(`import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const MARKER = %q;
const TOKEN = process.env.DONMAI_PI_HANDSHAKE ?? "";
const DESCRIPTORS = JSON.parse(%q);
const byName = new Map(DESCRIPTORS.map((d) => [d.name, d]));
const canonical = (v) => Array.isArray(v) ? "[" + v.map(canonical).join(",") + "]" : v && typeof v === "object" ? "{" + Object.keys(v).sort().map((k) => JSON.stringify(k) + ":" + canonical(v[k])).join(",") + "}" : JSON.stringify(v);
const digest = (v) => createHash("sha256").update(canonical(v)).digest("hex");
const sourceDigest = () => createHash("sha256").update(readFileSync(fileURLToPath(import.meta.url))).digest("hex");

export default function activate(pi: ExtensionAPI) {
  pi.on("session_start", (_event, ctx) => { void (async () => {
    const bind = {contractVersion:%q,kind:%q,token:TOKEN,deliveryId:%q,deliveryDigest:sourceDigest()};
    const raw = await ctx.ui.input(JSON.stringify(bind), MARKER);
    if (!raw) return;
    const response = JSON.parse(raw);
    if (response.contractVersion !== %q || !Array.isArray(response.tools)) return;
    const members = [];
    for (const name of response.tools) {
      const descriptor = byName.get(name); if (!descriptor) return;
      pi.registerTool({name, label:name, description:descriptor.description, parameters:descriptor.inputSchema,
        async execute(toolCallId, args, _signal, _onUpdate, callCtx) {
          const call = {contractVersion:%q,kind:%q,token:TOKEN,bindingDigest:response.bindingDigest,callId:String(toolCallId),name,arguments:args};
          const reply = await callCtx.ui.input(JSON.stringify(call), MARKER);
          if (!reply) return {content:[{type:"text",text:"code-intelligence call refused"}],isError:true};
          return JSON.parse(reply);
        }});
      members.push({name,schemaDigest:digest(descriptor.inputSchema)});
    }
    const inventory = {contractVersion:%q,kind:%q,token:TOKEN,bindingDigest:response.bindingDigest,configDigest:response.configDigest,members};
    await ctx.ui.input(JSON.stringify(inventory), MARKER);
  })(); });
}
`, ExtensionMarker, string(raw), ExtensionContractVersion, KindBind, DeliveryID, BindResponseVersion, ExtensionContractVersion, KindCall, ExtensionContractVersion, KindInventory))
}

// Delivery returns an independent copy of the fixed required Pi extension.
func Delivery() agent.ExtensionDelivery {
	source := extensionSource()
	sum := sha256.Sum256(source)
	return agent.ExtensionDelivery{ID: DeliveryID, Kind: agent.ExtensionDeliveryInline, Source: append([]byte(nil), source...), Basename: "donmai-code-intelligence.ts", Digest: hex.EncodeToString(sum[:]), Required: true}
}

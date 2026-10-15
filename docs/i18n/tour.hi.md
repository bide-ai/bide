# bide का परिचय

API, खंड दर खंड: ग्राफ़, ऐम्बिएंट रन, एजेंट लूप, स्ट्रीमिंग, टाइप्ड आउटपुट, सत्र, ऑडिट ट्रेल, मानवीय अनुमोदन, त्रुटियाँ, मिडलवेयर और गवर्नेंस। हर खंड उस गाइड से जुड़ता है जो उसे पूरी तरह समझाती है।

## Graphs

एक ग्राफ़-प्रथम फ़्रेमवर्क में ग्राफ़ वह है जिसे आप लिखते हैं और जो निष्पादित होता है, नोड, किनारों, एक अवस्था वस्तु, और कभी-कभी ऊपर एक दृश्य बिल्डर के साथ। bide में वही लेखन सतहें मौजूद हैं, एक दृश्य बिल्डर तक, एक सादे-Go जर्नल के ऊपर वैकल्पिक परतों के रूप में।

एक ग्राफ़ कोई अभिव्यंजक शक्ति नहीं जोड़ता। एक ग्राफ़ जो भी गणना करता है, सामान्य नियंत्रण-प्रवाह वही गणना करता है: एक अभिकलन ग्राफ़ एक नियंत्रण-प्रवाह ग्राफ़ ही है, और अनुक्रम, चयन, और पुनरावृत्ति उनमें से किसी को भी व्यक्त करने के लिए पर्याप्त हैं। कोई एजेंट व्यवहार ऐसा नहीं जिसे आप नोड-और-किनारे वाले ग्राफ़ के रूप में बना सकें पर `if`, `for`, और फ़ंक्शन से न लिख सकें। एक ग्राफ़ जो जोड़ता है वह प्रवाह का एक प्रथम-श्रेणी प्रतिनिधित्व है जिसे आप निरीक्षण, दृश्यांकन, स्थैतिक रूप से सत्यापन, और कोड के बाहर लिख सकते हैं। यह एक टूलिंग परत है, और एजेंट बनाने के लिए इसकी आवश्यकता नहीं।

तो यहाँ आधार सादा Go है, और गारंटियाँ (टिकाऊपन, ज़्यादा-से-ज़्यादा-एक-बार, सत्यापनीय ट्रेल) जर्नल से आती हैं। ग्राफ़ एक व्युत्पन्न दृश्य के रूप में मौजूद है: `RenderMermaid` उसे उसी से पुनर्निर्मित करता है जो वास्तव में चला।

ग्राफ़ लिखने के लिए `plan` पैकेज का उपयोग करें: एक सीमित, टाइप-जाँचा गया फ़्लो बिल्डर जो इस रनटाइम तक संकलित होता है और ज़्यादा-से-ज़्यादा-एक-बार तथा ऑडिट ट्रेल विरासत में लेता है। आप टाइप-किए गए नोडों (`Step`, `Tool`, `Model`, `Switch`, फ़ैन-इन `Join`, सीमाबद्ध `LoopBack`) को एक `Flow` में जोड़ते हैं, या उसी टोपोलॉजी को घोषणात्मक कॉन्फ़िग (`plan.Load`) के रूप में लिखते हैं जिसे एक उच्चतर परत जैसे कोई दृश्य बिल्डर उत्सर्जित कर सकती है। एक नोड m-of-n अनुमोदन गेट रख सकता है (`plan.Approval`, या कॉन्फ़िग में `approval: {need, approvers}`): फ़्लो उस नोड से पहले तब तक रुकता है जब तक k अनुमोदक उसके सटीक इनपुट पर हस्ताक्षर नहीं कर देते, उसी जर्नल किए गए गेट और ऑफ़लाइन साक्ष्य के साथ जो एजेंट के टूल कॉल के लिए है।

एक लिखा हुआ ग्राफ़ एक आरेख है; एक व्युत्पन्न ग्राफ़ जर्नल से पुनर्निर्मित है, इसलिए यह ठीक वही है जो चला। `plan` परत दोनों को जोड़ती है: `Topology()` और `RenderMermaid()` घोषित आकार को उजागर करते हैं, और `Conform()` क्रिप्टोग्राफ़िक रूप से जाँचता है कि एक रन ने उस टोपोलॉजी का पालन किया जिसे उसने घोषित किया। जो नियम किसी भी ऐसी परत को रनटाइम को फ़ोर्क करने से रोकता है वह यह है: एक नई सतह लिखने का एक तरीक़ा जोड़ सकती है, निष्पादन का कभी नहीं; हर परत उसी एक जर्नल-समर्थित रनटाइम तक उतरती है। देखें [docs/guides/plan-flows.md](../../docs/guides/plan-flows.md)।

### एक ही फ़्लो को लिखने के तीन तरीक़े

वही ऑर्डर-ट्राइएज फ़्लो, तीन तरीक़ों से। सादा Go डिफ़ॉल्ट है: सामान्य नियंत्रण-प्रवाह लिखें, और उन चरणों को नाम दें जिन्हें जर्नल को क्रैश-सुरक्षित बनाना है।

<!-- docsnip: setup ctx context.Context; journal *agent.Journal; order Order; type Order struct{}; type Receipt struct{}; type Assessment struct{ Rush bool }; type Reservation struct{}; func classify(Order) (Assessment, error); func reserve(Assessment) (Reservation, error); func finalize(Reservation) (Receipt, error); func decline(Assessment) (Receipt, error) -->
```go
// classify, then branch: rush orders reserve-then-finalize, the rest decline.
assess, _ := journal.Step(ctx, "order-42", "classify",
    func(ctx context.Context) (Assessment, error) { return classify(order) },
    agent.WithSafety(agent.Safety{ReadOnly: true})) // safe to re-run after a crash

var receipt Receipt
if assess.Rush {
    res, _ := journal.Step(ctx, "order-42", "reserve", // a side effect: at most once
        func(ctx context.Context) (Reservation, error) { return reserve(assess) })
    receipt, _ = journal.Step(ctx, "order-42", "finalize",
        func(ctx context.Context) (Receipt, error) { return finalize(res) })
} else {
    receipt, _ = journal.Step(ctx, "order-42", "decline",
        func(ctx context.Context) (Receipt, error) { return decline(assess) })
}
```

उस फ़्लो को एक निरीक्षणीय कलाकृति बनाने के लिए, `plan` बिल्डर टाइप-किए गए नोडों को एक `Flow` में जोड़ता है जो उसी रनटाइम पर उतरता है:

<!-- docsnip: setup type Order struct{}; type Receipt struct{}; type Assessment struct{ Rush bool }; type Reservation struct{} -->
```go
b := plan.New[Order, Receipt]("order-triage")
classify := b.Step("classify", func(ctx context.Context, o Order) (Assessment, error) { ... })
reserve  := b.Step("reserve",  func(ctx context.Context, a Assessment) (Reservation, error) { ... }) // non-idempotent
finalize := b.Step("finalize", func(ctx context.Context, r Reservation) (Receipt, error) { ... })
decline  := b.Step("decline",  func(ctx context.Context, a Assessment) (Receipt, error) { ... })

b.Switch(classify,
    plan.When(func(a Assessment) bool { return a.Rush }, reserve).Named("rush"),
    plan.Else(decline),
)
b.Edge(reserve, finalize)

flow, err := b.Build() // inherits at-most-once and the audit trail
```

या उसी टोपोलॉजी को घोषणात्मक कॉन्फ़िग के रूप में लिखें जिसे एक उच्चतर परत (एक दृश्य बिल्डर) उत्सर्जित कर सकती है, और `plan.Load` से लोड करें:

```json
{
  "version": 1,
  "flow": "order-triage",
  "entry": "classify",
  "nodes": [
    {"name": "classify", "block": "classify"}, {"name": "reserve", "block": "reserve"},
    {"name": "finalize", "block": "finalize"}, {"name": "decline", "block": "decline"}
  ],
  "wiring": [
    {"switch": "classify", "when": [{"pred": "rush", "to": "reserve"}], "else": "decline"},
    {"edge": ["reserve", "finalize"]}
  ]
}
```

<!-- docsnip: setup type Order struct{}; type Receipt struct{}; configBytes []byte; reg *plan.Registry -->
```go
flow, err := plan.Load[Order, Receipt](configBytes, reg) // same topology, same Digest()
```

तीनों उसी एक जर्नल-समर्थित रनटाइम तक उतरते हैं, इसलिए आप कोई भी चुनें, ज़्यादा-से-ज़्यादा-एक-बार, HA पुनरारंभ, और सत्यापनीय ट्रेल लागू होते हैं।

## एंबिएंट रन: टिकाऊ नींद, जागना, और अंतरायण

एक एंबिएंट रन एक समकालिक चैट लूप में नहीं बैठता। यह सोता है, एक ट्रिगर पर जागता है, और एक इंसान के लिए रुकता है, और उन संक्रमणों में से हर एक जर्नल पर एक टिकाऊ, ज़्यादा-से-ज़्यादा-एक-बार वाला चरण है, ताकि रन उनके बीच क्रैश और नोड-हस्तांतरण से बच जाए।

- **एक समय-सीमा तक सोना।** `Sleep`/`WaitUntil` एक रन को रोकते हैं और उसका जागने का समय जर्नल में लिखते हैं, ताकि विराम एक पुनरारंभ को जी ले। जागने के समय पर पुनः-आह्वान ठीक एक बार पुनरारंभ करता है। (यह एजेंट के टूल के भीतर चलता है, `plan` फ़्लो के नोड में अभी नहीं।)
- **समय या घटना पर जागना।** एक प्लग-करने-योग्य `Waker` (डिफ़ॉल्ट रूप से इन-प्रोसेस `MemWaker`) एक देय रन को पुनः-आह्वान करता है; ट्रिगर स्रोत आपका है (एक इन-प्रोसेस लूप, एक cron, एक क़तार, एक इनबाउंड webhook), ताकि वही आधार शेड्यूल-किए और घटना-चालित दोनों एजेंटों को चलाए।
- **एक इंसान के लिए, टिकाऊ रूप से अंतरायित करना।** `Interrupt[T]`/`AnswerInterrupt` एक रन को किसी भी बिंदु पर रोककर एक टाइप-किए गए निर्णय का अनुरोध करते हैं और इंसान के उत्तर के साथ एक जर्नल-किए चरण के रूप में पुनरारंभ करते हैं (देखें [Human-in-the-loop](#human-in-the-loop))। अनुमोदित/अस्वीकृत उस बूलियन का विशेष मामला है।

आप ट्रिगर स्रोत और निगरानी सतह देते हैं; रनटाइम रन को हर नींद, जागरण, अंतरायण, क्रैश, और हस्तांतरण के आर-पार सही बनाए रखता है। `examples/signals` (एक प्रतीक्षारत रन में एक घटना पहुँचाएँ), `examples/interrupt` (human-in-the-loop विराम/पुनरारंभ), और `examples/recover` (टिकाऊ पुनरारंभ) में चलाने योग्य। देखें [सिग्नल और एंबिएंट गाइड](../../docs/guides/signals.md)।

## ज़्यादा-से-ज़्यादा एक बार, कोड में

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string; type ChargeArgs struct{}; type Receipt struct{} -->
```go
// A tool that moves money is a write: not ReadOnly, not Idempotent.
charge := agent.MustFunc("charge_card", "Charge the customer", func(ctx context.Context, in ChargeArgs) (Receipt, error) { /* ... */ })

// If the process crashes after the charge fires but before its result is journaled,
// resume does NOT run it again: it returns *OutcomeUnknown so you confirm, not double-charge:
_, err := a.Run(ctx, runID, agent.UserText(input))
if halt, ok := errors.AsType[*agent.OutcomeUnknown](err); ok {
	// halt.Op.ToolName == "charge_card": outcome unknown, a human decides, no double side effect.
}
```

## त्वरित शुरुआत

Go 1.27 की आवश्यकता है (कोर जेनेरिक मेथड इस्तेमाल करता है)। यदि `go version` पुराना है, तो अपग्रेड करें या `GOTOOLCHAIN=go1.27.0` सेट करें।

कोर पैकेज `agent` है, जो `github.com/bide-ai/bide/agent` से import होता है (जैसा नीचे का ब्लॉक दिखाता है)। अपने मॉड्यूल में इसे और SQLite स्टोर को `go get github.com/bide-ai/bide/agent@latest github.com/bide-ai/bide/store/sqlite@latest` से इंस्टॉल करें (या कोड लिखकर `go mod tidy` चलाएँ)। API key नहीं है? [शुरुआती गाइड](../getting-started.md#no-api-key-use-agenttest) वही agent एक scripted model के साथ offline चलाती है।

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/openai"
	"github.com/bide-ai/bide/store/sqlite"
)

type WeatherArgs struct {
	City string `json:"city" desc:"city name"`
}
type Weather struct {
	TempF int    `json:"temp_f"`
	Sky   string `json:"sky"`
}

func main() {
	// Any OpenAI-compatible endpoint (here OpenRouter); swap the base URL for Ollama, etc.
	model := openai.New(os.Getenv("OPENROUTER_API_KEY"),
		openai.WithBaseURL("https://openrouter.ai/api/v1"),
		openai.WithModel("openai/gpt-4o-mini"))

	// A tool is a typed Go function; its schema is derived automatically.
	weather := agent.MustFunc("get_weather", "Current weather for a city",
		func(_ context.Context, in WeatherArgs) (Weather, error) {
			return Weather{TempF: 68, Sky: "sunny"}, nil
		}, agent.WithSafety(agent.Safety{ReadOnly: true}))

	// Durable on-disk store: a crash mid-run resumes from here.
	store, err := sqlite.Open("agent.db")
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	j, err := agent.NewJournal(store)
	if err != nil {
		log.Fatal(err)
	}

	a, err := agent.New(
		model,
		j,
		agent.WithTools(weather),
		agent.WithSystemPrompt("You are a concise weather assistant."),
	)
	if err != nil {
		log.Fatal(err)
	}
	out, err := a.Run(context.Background(), "run-1", agent.UserText("Weather in SF? Use the tool."))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out.Message.Text())
}
```

लाइव स्मोक उदाहरण चलाएँ: `OPENROUTER_API_KEY=sk-... go run github.com/bide-ai/bide/examples/smoke@latest`

`Run` एक `Message` इनपुट (पाठ, या पाठ और चित्र) और प्रति-रन विकल्प लेता है, और एक `Result` लौटाता है: अंतिम संदेश, पूरे रन का टोकन उपयोग (कैश और सब-एजेंटों सहित), मॉडल-ट्रन गिनती, और वॉल-क्लॉक अवधि। `Turns` केवल इस invocation के मॉडल टर्न गिनता है, इसलिए फिर से शुरू किया गया रन, या फिर से चलाया गया पूरा हो चुका रन, पूरे रन से कम टर्न दिखा सकता है (`Usage` और `Spend` पूरे रन के होते हैं)। रन ID मान्य होने पर `Result` हर त्रुटि पर भी nil नहीं होता: एक ठहराव, एक रुकावट, एक विफलता, एक सागा का निरस्तीकरण, एक रद्दीकरण:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string -->
```go
res, err := a.Run(ctx, runID, agent.UserText(input),
	agent.WithTokenBudget(50_000), agent.WithSystemPrompt("You are terse."))
// res.Message, res.Usage, res.Spend, res.Turns, res.Duration, res.RunID
_ = err
```

रन की पहली ड्राइव अपना इनपुट और विकल्प `run:start` में जर्नल करती है, और हर बाद की ड्राइव, रिकवरी सहित, उन्हीं के तहत चलती है: एक अलग सीमा (`WithMaxTurns`, `WithTokenBudget`) एक संशोधन `run:limits:<n>` के रूप में जर्नल होती है, और कोई अन्य अलग सेटिंग `ErrConfig` है। `agent.Cancel` एक रन रद्द करता है (एक सागा पहले वापस घुमाई जाती है), और `agent.Status` जर्नल से उसकी स्थिति पढ़ता है।

## स्ट्रीमिंग

`Run` ब्लॉक करता है और अंतिम उत्तर लौटाता है। एजेंट को काम करते देखने के लिए (टोकन डेल्टा, ट्रन सीमाएँ, टूल शुरू/समाप्त), `Stream` इस्तेमाल करें। यह **वही लूप** चलाता है (`Stream(...).Result()` वही लौटाता है जो `Run` लौटाता है), इसलिए टिकाऊपन, पुनरारंभ, और साइड-इफ़ेक्ट सुरक्षा समरूप हैं:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string -->
```go
stream := a.Stream(ctx, runID, agent.UserText(input))
for ev := range stream.Events() {
	switch e := ev.(type) {
	case agent.ModelEvent: // live token/reasoning/tool-call deltas
		if d, ok := e.Event.(agent.TextDelta); ok {
			fmt.Print(d.Text)
		}
	case agent.ToolStarted:
		fmt.Printf("\n[calling %s]\n", e.Name)
	case agent.ToolCompleted:
		fmt.Printf("[%s done]\n", e.Name)
	}
}
res, err := stream.Result() // what Run returns: the Result and an error (incl. a pause: *ApprovalPending, *OutcomeUnknown, ...)
```

घटनाएँ: `TurnStarted`, `ModelEvent` (टोकन फ़ीड), `AssistantTurn`, `ToolStarted` / `ToolCompleted`, `ApprovalRequired`, `Finished`। एक UI के लिए `Events()` पर रेंज करें फिर `Result()` को कॉल करें, या अकेले `Result()` कॉल करें ताकि यह ठीक `Run` की तरह व्यवहार करे (यह आपके लिए घटनाओं को निकाल देता है)।

दो बातें जानने योग्य, दोनों टिकाऊपन के परिणाम:
- **टोकन डेल्टा middleware शृंखला के नीचे पहुँचते हैं** (Retry / Cost फिर भी पूरे संयोजित संदेश देखते हैं), और **केवल एक ताज़ा मॉडल कॉल पर**।
- **पुनरारंभ पर, जर्नल-किया गया ट्रांसक्रिप्ट फिर से उत्सर्जित होता है** `AssistantTurn{Replayed: true}` + `ToolCompleted` के रूप में, लाइव प्रगति से पहले, ताकि एक ताज़ा UI क्रैश के बाद पूरी कहानी पुनर्निर्मित कर ले, और एक पुनर्खेल-किया गया ट्रन कोई टोकन डेल्टा उत्पन्न नहीं करता (वह पहले ही तय हो चुका था)।

`Stream` वही विकल्प लेता है जो `Run` (सागा के लिए `WithSaga()`), और उसका `RunStream.Result()` वही लौटाता है जो `Run` लौटाता।

## टाइप-किया गया आउटपुट

`RunTyped[T]` एक मुक्त-रूप संदेश के बजाय एक टाइप-किया गया `T` लौटाता है। यह एक कृत्रिम `final_answer` टूल इंजेक्ट करता है जिसका JSON स्कीमा `T` से व्युत्पन्न है (`schema` पैकेज के माध्यम से) और मॉडल को उसका काम पूरा होने पर इसे एक बार कॉल करने की ओर संचालित करता है, ताकि एक टूल-उपयोगी एजेंट अपना काम कर सके और फिर टाइप-किया हुआ उत्तर दे। यह प्रदाता-अज्ञेय है: यह नेटिव टूल कॉलिंग का उपयोग करता है, किसी प्रदाता के JSON मोड का नहीं।

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string -->
```go
type Weather struct {
	City  string `json:"city"`
	TempF int    `json:"temp_f"`
}

w, _, err := a.RunTyped[Weather](ctx, runID, agent.UserText("weather in SF?"))
// w.City == "SF", w.TempF == 68
```

यह एक Go 1.27 जेनेरिक मेथड है; यह रन का `Result` भी लौटाता है (उसका `Output` उत्तर है, जैसा जर्नल किया गया)। मान *जर्नल-किए गए* टूल कॉल से डीकोड होता है, इसलिए यह **पुनरारंभ-सुरक्षित** है: एक रन के बीच का क्रैश पुनरारंभ पर लॉग से टाइप-किया गया उत्तर वापस पा लेता है। पहली `final_answer` कॉल जिसे टूल स्वीकार करता है, रन को समाप्त कर देती है। केवल यदि मॉडल ऐसी कोई कॉल कभी नहीं करता (वह इसके बजाय सादे JSON टेक्स्ट में उत्तर देता है), तभी `RunTyped` रन के अंतिम ट्रन के टेक्स्ट को पार्स करता है। `T` को एक JSON ऑब्जेक्ट होना चाहिए (एक struct, उसका पॉइंटर, या एक map), क्योंकि प्रदाता टूल आर्ग्युमेंट केवल ऑब्जेक्ट के रूप में लेते हैं; कोई भी अन्य `T` `ErrConfig` है।

सख्त संरचित आउटपुट वाले OpenAI-संगत प्रदाताओं पर, `agent.WithOutputMode(agent.OutputNative)` के साथ `RunTyped[T]` टूल के बजाय प्रदाता के नेटिव JSON-स्कीमा प्रतिक्रिया प्रारूप का उपयोग करता है (स्कीमा प्रदाता-पक्ष पर प्रवर्तित, कोई टूल राउंड-ट्रिप नहीं); Anthropic अडैप्टर इसका समर्थन नहीं करता और `ErrConfig` लौटाता है, इसलिए वहाँ प्रदाता-अज्ञेय आउटपुट के लिए डिफ़ॉल्ट टूल मोड इस्तेमाल करें।

## सैंपलिंग

जनन नियंत्रण प्रदाता-निरपेक्ष हैं और एक बार सेट होते हैं; हर अडैप्टर उन्हें अपने वायर प्रारूप पर मैप करता है (और जो वह नहीं कर सकता उसे गिरा देता है, जैसे Anthropic के पास `seed` नहीं है):

<!-- docsnip: setup model agent.Model; journal *agent.Journal; tools []agent.Tool -->
```go
a, err := agent.New(
	model,
	journal,
	agent.WithTools(tools...),
	agent.WithSampling(agent.Temperature(0), agent.MaxTokens(500), agent.TopP(0.9), agent.Seed(42)),
)
if err != nil {
	panic(err)
}
```

फ़ील्ड वैकल्पिक हैं: एक अनसेट फ़ील्ड प्रदाता डिफ़ॉल्ट इस्तेमाल करता है, इसलिए एक स्पष्ट `Temperature(0)` "निर्दिष्ट नहीं" से भिन्न है। अनुरोध-स्तरीय `MaxTokens` एक अडैप्टर के निर्माण-समय डिफ़ॉल्ट को ओवरराइड करता है।

## प्रॉम्प्ट कैशिंग

एक एजेंट लूप हर ट्रन एक बड़ा स्थिर उपसर्ग (सिस्टम प्रॉम्प्ट + टूल स्कीमा) दोबारा भेजता है। Anthropic प्रॉम्प्ट कैशिंग उन दोहरावों को कैश-रीड दर पर बिल करता है:

<!-- docsnip: setup key string -->
```go
model := anthropic.New(key, anthropic.WithPromptCache())
```

यह सिस्टम ब्लॉक और टूल परिभाषाओं पर `cache_control` ब्रेकपॉइंट रखता है। OpenAI उपसर्गों को स्वचालित रूप से कैश करता है (कोई फ़्लैग नहीं चाहिए)। किसी भी तरह, कैश प्रभावशीलता `agent.Usage` में उभरती है (`CacheReadTokens`, कैश से परोसे गए, और `CacheWriteTokens`, उसमें लिखे गए), ताकि लागत लेखांकन, ट्रेसिंग, और रन का टोकन बजट असली संख्याएँ देखें।

## सत्र (बहु-ट्रन)

`Run` एक ट्रन है। एक `Session` एक टिकाऊ बहु-ट्रन बातचीत है: हर `Send` एक पूर्ण एजेंट रन है (टूल, पुनरारंभ, साइड-इफ़ेक्ट सुरक्षा) जो अब तक के ट्रांसक्रिप्ट से बीजित है, ताकि एजेंट पिछले ट्रन याद रखे।

<!-- docsnip: setup ctx context.Context; a *agent.Agent -->
```go
s, _ := a.Session(ctx, "user-42")   // reopens + rebuilds the transcript from the store
r1, err := s.Send(ctx, agent.UserText("what's the capital of France?"))
if err != nil {
	panic(err)
}
r2, err := s.Send(ctx, agent.UserText("and its population?")) // sees turn 1 in context
if err != nil {
	panic(err)
}
fmt.Println(r1.Message.Text(), r2.Message.Text())
```

ट्रांसक्रिप्ट सत्र id के तहत ट्रन-दर-ट्रन जर्नल होता है, ताकि एक पुनरारंभ हुआ प्रोसेस `a.Session(ctx, "user-42")` इसे पुनर्निर्मित करके जारी रखे। ट्रन N `"<id>>@turn/N"` के तहत चलता है (उसका अपना टिकाऊ जर्नल एक ट्रन के *भीतर* क्रैश-पुनरारंभ संभालता है); वार्तालाप स्मृति प्रश्न/उत्तर ट्रांसक्रिप्ट है: एक ट्रन के मध्यवर्ती टूल कॉल उस ट्रन में ही रहते हैं और बाद के ट्रनों में नहीं रिसते। यदि एक ट्रन रुकता है (अनुमोदन / `Interrupt`), तो `Send` वह त्रुटि लौटाता है; उसे हल करें और पुनरारंभ के लिए उसी इनपुट के साथ `Send` फिर कॉल करें। तब तक, किसी भिन्न संदेश के साथ `Send` `ErrConfig` लौटाता है: खुला ट्रन अपने संदेश का है। उन इनबाउंड संदेशों के लिए जो दोबारा पहुँचाए जा सकते हैं, `SendOnce(ctx, id, text)` हर संदेश id का उत्तर एक बार देता है। एक सत्र पर कई हैंडल कभी कोई ट्रन नहीं खोते, किसी ट्रन को दो बार दर्ज नहीं करते, न ही एक संदेश का उत्तर किसी दूसरे के जवाब से देते हैं। लीज़ वाले स्टोर (`MemStore`, SQLite, Postgres) पर एक ट्रन अपने रन की लीज़ के तहत एक समय में एक ही वर्कर द्वारा चलाया जाता है, इसलिए उसका टोकन बजट कई वर्करों में भी बना रहता है; इस बीच वही संदेश पाने वाले दूसरे वर्कर को `ErrTurnContended` मिलता है और वह उसे बाद में फिर भेजता है। रद्द किए गए रन वाला Send ट्रन बंद दर्ज होता है: उस संदेश का `Send` `ErrRunCancelled` लौटाता है, और अगले संदेश का `Send` उस ट्रन को बिना उत्तर के बंद दर्ज करके अपना ट्रन चलाता है।

## ऑडिट-योग्यता (छेड़छाड़-स्पष्ट जर्नल)

टिकाऊ जर्नल पहले से एक रन के हर चरण को रिकॉर्ड करता है। `audit` पैकेज उस इतिहास से एक हैश शृंखला के साथ प्रतिबद्ध होता है, ताकि एक रन का निष्पादन सत्यापनीय हो:

<!-- docsnip: setup ctx context.Context; journal *agent.Journal; runID string; priv ed25519.PrivateKey -->
```go
head, _ := audit.Head(ctx, journal, runID)                     // SHA-256 chain over the stored journal bytes
sig, _ := audit.Sign(head, audit.Ed25519Signer{Priv: priv}) // anchor it: sign / publish out-of-band
```

एक रिकॉर्ड का कोई भी संशोधन / प्रविष्टि / विलोपन / पुनः-क्रमण हेड को बदल देता है। **सुरक्षा मॉडल:** यह अखंडता बिना शर्त देता है, और छेड़छाड़-स्पष्टता *तब जब आप हेड को बैंड-से-बाहर एंकर करते हैं* (एक ही DB में एक शृंखला जिसे एक हमलावर नियंत्रित करता है, दोबारा लिखी और दोबारा हैश की जा सकती है); पैकेज दस्तावेज़ देखें।

**चयनात्मक प्रकटीकरण** के लिए, `audit.Root` / `Prove` / `VerifyInclusion` एक **RFC 6962** (Certificate Transparency) Merkle ट्री बनाते हैं, ताकि आप एक O(log n) समावेशन प्रमाण के माध्यम से सिद्ध कर सकें कि एक रिकॉर्ड एक प्रतिबद्ध रन का भाग है, बाकी रिकॉर्ड उजागर किए बिना (जैसे एक ऑडिटर को दिखाएँ कि एक चार्ज हुआ, कोई अन्य ग्राहक या प्रॉम्प्ट प्रकट किए बिना)। और `ProveConsistency` / `VerifyConsistency` सिद्ध करते हैं कि एक पूर्ववर्ती रूट एक बाद वाले का **append-only उपसर्ग** है: कि इतिहास में केवल जोड़ा गया, कभी दोबारा लिखा या पुनः-क्रमबद्ध नहीं किया गया (ट्रांसपेरेंसी-लॉग गारंटी)। कार्यान्वयन प्रकाशित RFC 6962 परीक्षण वेक्टरों के विरुद्ध जाँचा गया है।

`SignTreeHead` CT-शैली का **हस्ताक्षरित ट्री हेड (Signed Tree Head)** उत्पन्न करता है, `{Kind, RunID, Size, Root, TimestampNanos}` जो अपनी हस्ताक्षर योजना सहित किसी भी `audit.Signer` (Ed25519, ECDSA P-256, ML-DSA-65, या दोनों का हाइब्रिड) से हस्ताक्षरित है, वह कलाकृति जिसे आप प्रकाशित करते हैं। पूरा प्रवाह: एक STH पर हस्ताक्षर करें, बाद में एक समावेशन प्रमाण के साथ एक एकल रिकॉर्ड प्रकट करें जिसे एक ऑडिटर हस्ताक्षरित रूट के विरुद्ध जाँचता है, और दो STH के बीच append-only वृद्धि सिद्ध करें। मॉडल, API, और एंड-टू-एंड अनुपालना प्रवाह के लिए [docs/guides/audit.md](../../docs/guides/audit.md) देखें।

### रन व्यूअर

`bide view` एक रन के लिए केवल-पढ़ने वाला पेज खोलता है, `127.0.0.1` पर, SQLite या Postgres जर्नल या निर्यात किए गए
जर्नल से। इंस्टॉल करें: `go install github.com/bide-ai/bide/cmd/bide@latest`। `bide view -config flow.yaml` इसी तरह फ़्लो की परिभाषा दिखाता है और समस्याओं को ग्राफ़ पर चिह्नित करता है; `-check` यही जाँच CI में चलाता है। `bide check flow.yaml -diff main` फ़्लो के दो संस्करणों की तुलना ग्राफ़ पर दिखाता है (जोड़े गए हरे, हटाए गए धराशायी लाल, बदले गए एम्बर में) और कमज़ोर हुई अनुमोदन नीति (गेट हटाया गया, उसकी आवश्यकता घटी, `exclude_principal` हटा, या `timeout` हटाया या लंबा किया गया) को अलग से बताता है। पेज हर नोड को इस आधार पर रंगता है कि क्रैश उसके साथ क्या करता है (साइड इफ़ेक्ट रुकता है, रीड-ओनली या idempotent नोड फिर से चलता है), और कॉन्फ़िग की `assert` पंक्तियों, जैसे `every path to charge passes approval >= 2 of 3`, का निर्णय करता है; उल्लंघन का प्रति-उदाहरण पथ लाल रंग में दिखता है। यह रन की समय-रेखा (मॉडल टर्न, टूल कॉल और उनकी `Safety`, प्रयास, अनुमोदन), रुका हुआ ऑपरेशन
और उसका परिणाम दर्ज करने वाला `ResolveHalt` कॉल, हर अनुमोदन गेट की k-of-n स्थिति, घोषित टोपोलॉजी पर
`plan` फ़्लो का पथ, और यह दिखाता है कि जर्नल हस्ताक्षरित ट्री हेड या साक्ष्य पैकेज के विरुद्ध सत्यापित
होता है या नहीं। यह स्टोर में कभी नहीं लिखता। जब तक रन चल रहा है, उसका पेज (SQLite या Postgres जर्नल से) नए रिकॉर्ड आते ही अपने-आप अपडेट होता है, रन समाप्त होने तक। `-run` के बिना यह रनों की सूची खोलता है, जिसे स्थिति, प्रकार और रन ID से छाना जा सकता है। देखें [रन व्यूअर गाइड](../../docs/guides/run-viewer.md) (अंग्रेज़ी में)।

<table>
<tr>
<td width="50%"><picture><source media="(prefers-color-scheme: dark)" srcset="../../docs/guides/img/refund-before-dark.png"><img src="../../docs/guides/img/refund-before-light.png" width="100%" alt="bide view: refund-42, before"></picture></td>
<td width="50%"><picture><source media="(prefers-color-scheme: dark)" srcset="../../docs/guides/img/refund-after-dark.png"><img src="../../docs/guides/img/refund-after-light.png" width="100%" alt="bide view: refund-42, after"></picture></td>
</tr>
<tr>
<td>पहले: रिफ़ंड चलने के बाद क्रैश हुआ, इसलिए रन रुका है और परिणाम अज्ञात है; पेज <code>ResolveHalt</code> कॉल देता है।</td>
<td>बाद में: <code>ResolveHalt</code> ने परिणाम दर्ज किया और रन पूरा हुआ; गेट 3 में से 2 अनुमोदक दिखाता है, और साक्ष्य पैकेज सत्यापित होता है।</td>
</tr>
</table>

## RAG और स्मृति (अपनी लाएँ)

Bide कोई वेक्टर स्टोर, एम्बेडर, या स्मृति बैकएंड नहीं देता: यह आपको इंटरफ़ेस देता है और आप वह स्टोर प्लग करते हैं जिसे आप पहले से चलाते हैं। अपने अवसंरचना के विरुद्ध एक इंटरफ़ेस लागू करें:

<!-- docsnip: api agent -->
```go
type Retriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]agent.Doc, error)
}
```

फिर इसे दो में से एक तरीक़े से जोड़ें:

<!-- docsnip: setup model agent.Model; journal *agent.Journal; myStore agent.Retriever -->
```go
// Agentic RAG: the model searches on demand:
a, err := agent.New(model, journal,
	agent.WithTools(agent.MustRetrievalTool("search_kb", "Search the knowledge base.", myStore, 5)))

// Classic RAG: top-k auto-injected as context on each user turn:
a, err = agent.New(model, journal, agent.WithRetrieval(myStore, 5))
```

वार्तालाप स्मृति पहले से अंतर्निहित है (`Session`); गतिशील संदर्भ `WithSystemPromptFunc` से गुज़रता है; यह सीवन शब्दार्थ / दीर्घकालिक स्मृति को समेटता है। मूर्त स्टोर अडैप्टर (यदि कभी आवश्यक हों) अलग मॉड्यूल होंगे, कभी कोर में नहीं। देखें [docs/guides/rag-memory.md](../../docs/guides/rag-memory.md)।

## पुनरारंभ सुरक्षा, एक तालिका में

```go
agent.Safety{ReadOnly: true}          // no side effects → always safe to re-run
agent.Safety{Idempotent: true}        // safe to retry (dedupes downstream)
agent.Safety{}                        // a write → HALT on unknown outcome, don't double-fire
agent.WithApproval(agent.SingleApproval()) // not Safety: a tool option that pauses for human approval before executing
```

एक non-idempotent साइड इफ़ेक्ट से पहले लूप एक टिकाऊ *प्रयास चिह्न* रिकॉर्ड करता है, ताकि पुनरारंभ "कभी नहीं चला" (चलाना सुरक्षित) को "चला और क्रैश हुआ" (रुको) से बता सके।

`dst_test.go` एक नियतात्मक अनुकरण परीक्षण है: एक दोष-इंजेक्ट करने वाला स्टोर *हर* राइट-बिंदु पर क्रैश करता है (और सैकड़ों यादृच्छिकृत बहु-क्रैश शेड्यूलों के आर-पार), और हार्नेस अभिकथन करता है कि एक non-idempotent साइड इफ़ेक्ट हर बार **ज़्यादा-से-ज़्यादा एक बार** चलता है, और रन हमेशा completed या halted पर समाप्त होता है, कभी दो बार नहीं चलता।

हार्नेस निर्यातित है (`chaos/`) और `benchmarks/` में अन्य SDK पर इंगित है। मापा गया परिणाम: **Bide `maxFired=1` (PASS); trpc-agent-go `maxFired=6`; langchaingo `maxFired=64` (दोनों FAIL)।** trpc का चेकपॉइंट/पुनरारंभ काम करता है (एक पूर्ण रन को पुनरारंभ करना एक no-op है); उसका डबल-फ़ायर वह प्रलेखित LangGraph "नोड idempotent होने चाहिए" खिड़की है। langchaingo में टिकाऊपन नहीं है, इसलिए पुनः-प्रयास सब कुछ दोबारा चलाते हैं। Bide का प्रयास-चिह्न उस खिड़की को बंद करता है।

`WithMaxTurns(n)` प्रति रन मॉडल ट्रनों की एक ऊपरी सीमा लगाता है ताकि एक मॉडल जो टूल कॉल करता रहता है सदा के लिए लूप न कर सके: इस तक पहुँचना `ErrMaxTurns` लौटाता है (जो `errors.Is` `ErrBudget` है)। `WithTokenBudget(n)` उन टोकनों की सीमा लगाता है जो एक रन इस्तेमाल कर सकता है, कैश किया गया इनपुट सहित: एक बार रन `n` इस्तेमाल कर ले, तो वह कोई और मॉडल कॉल नहीं करता और `ErrBudgetExceeded` लौटाता है। हर कॉल का उपयोग उसके ट्रन के साथ जर्नल होता है, इसलिए दोनों सीमाएँ जर्नल से पुनर्निर्मित होती हैं और एक क्रैश और पुनरारंभ के आर-पार टिकी रहती हैं।

## Human-in-the-loop

तीन रूप। **अनुमोदन/अस्वीकृति**: `WithApproval(SingleApproval())` से चिह्नित एक टूल चलने से *पहले* रुकता है; इंसान का निर्णय एक bool है:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; journal *agent.Journal; runID string; input string -->
```go
_, err := a.Run(ctx, runID, agent.UserText(input))
if pend, ok := errors.AsType[*agent.ApprovalPending](err); ok {
	// ... get a human decision ...
	agent.Approve(ctx, journal, pend.RunID, pend.ToolUseID, true)
	res, _ := a.Run(ctx, pend.RootRunID, agent.UserText(input))
	var out agent.Message
	if res != nil {
		out = res.Message
	} // resumes past the pause
}
```

**अंतरायण/पुनरारंभ**: एक टूल *एक मनमाने बिंदु पर* रुकता है और एक *टाइप-किए गए* मान के साथ पुनरारंभ होता है (bool का सामान्यीकरण)। एक पुनः-प्रयास-सुरक्षित टूल के भीतर `agent.Interrupt[T]` कॉल करें:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; journal *agent.Journal; runID string; input string; type Options struct{}; type Plan struct{}; chosenPlan Plan -->
```go
tool := agent.MustFunc("choose_plan", "pick a plan", func(ctx context.Context, in Options) (Plan, error) {
		pick, err := agent.Interrupt[Plan](ctx, "plan", in) // pauses the run; in is shown to the human
		if err != nil {
			return Plan{}, err // *InterruptPending propagates out of Run
		}
		return pick, nil // on resume, pick is the human's typed answer
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))

_, err := a.Run(ctx, runID, agent.UserText(input))
if intr, ok := errors.AsType[*agent.InterruptPending](err); ok {
	// ... show intr.Prompt, get a typed answer ...
	journal.AnswerInterrupt(ctx, intr.RunID, intr.Name, chosenPlan)
	res, _ := a.Run(ctx, intr.RootRunID, agent.UserText(input))
	var out agent.Message
	if res != nil {
		out = res.Message
	} // resumes; Interrupt now returns chosenPlan
}
```

दोनों टिकाऊ हैं: निर्णय/मान एक जर्नल-किया गया चरण है, इसलिए यह एक क्रैश को जी लेता है। Interrupt को एक पुनः-प्रयास-सुरक्षित टूल में होना चाहिए (`ReadOnly`/`Idempotent`): पुनरारंभ पर टूल तब तक दोबारा चलता है जब तक अंतरायण हल न हो, इसलिए `Interrupt` कॉल से पहले सब कुछ दोहराने के लिए सुरक्षित होना चाहिए।

**m-of-n अनुमोदन**: जब एक हस्ताक्षर-स्वीकृति पर्याप्त नहीं, तो n अनुमोदकों के एक नामित समूह से k हस्ताक्षरित निर्णय आवश्यक करें। हर अनुमोदक ठीक उसी कॉल (टूल और तर्कों) पर हस्ताक्षर करता है; गेट k अनुमोदनों पर आगे बढ़ता है, k अप्राप्य होते ही अस्वीकार करता है, और अन्यथा चालू गणना के साथ रुकता है। एक जाली या ग़लत निर्णय अनदेखा किया जाता है, उसके अनुमोदक को बाहर किए बिना:

<!-- docsnip: setup ctx context.Context; model agent.Model; journal *agent.Journal; pend *agent.ApprovalPending; type RefundArgs struct{}; doRefund func(context.Context, RefundArgs) (string, error); keysByApprover agent.ApproverVerifierFor; signer audit.Signer -->
```go
refund := agent.MustFunc("refund", "refund the order", doRefund,
	agent.WithApproval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}))
a, err := agent.New(model, journal, agent.WithTools(refund), agent.WithApproverVerifiers(keysByApprover))
if err != nil {
	panic(err)
}

// each approver, out of band, signs the paused call they were shown:
decidedAt := time.Now() // the approver's clock, signed with the decision
sig, _ := signer.Sign(agent.ApprovalDecisionBytes(pend.Subject(), "finance", true, decidedAt))
agent.SubmitDecision(ctx, journal, agent.Decision{RunID: pend.RunID, ToolUseID: pend.ToolUseID,
	ApproverID: "finance", Approved: true, DecidedAt: decidedAt, Alg: signer.Alg(), Signature: sig})
```

फिर `audit.ApprovalEvidence` और `audit.VerifyApprovals` (या `bide audit verify-approvals`) ऑफ़लाइन सिद्ध करते हैं कि k नामित अनुमोदकों ने ठीक इसी कॉल पर उसके चलने से *पहले*, अपेक्षित नीति के तहत, हस्ताक्षर-स्वीकृति दी, ऐसे साक्ष्य से जो किसी निर्णय को बिना पकड़े छोड़ नहीं सकता। हर अनुमोदक की अपनी कुंजी होनी चाहिए: जिस नीति के दो अनुमोदक एक ही कुंजी पर पहुँचते हैं, उसे `ErrConfig` के साथ अस्वीकार किया जाता है, क्योंकि उस कुंजी का धारक दोनों की ओर से हस्ताक्षर कर सकता है। देखें [अनुमोदन गाइड](../../docs/guides/hitl-approval.md); अलग-अलग प्रोसेसों के आर-पार `examples/approval` में चलाने योग्य।

**कर्तव्यों का पृथक्करण**: `ExcludePrincipal` सेट करें तो गेट रन के अनुरोधकर्ता का निर्णय कभी नहीं गिनता, इसलिए जिसने कार्रवाई शुरू की वह उसे अनुमोदित भी नहीं कर सकता (maker-checker):

<!-- docsnip: setup type RefundArgs struct{}; doRefund func(context.Context, RefundArgs) (string, error) -->
```go
refund := agent.MustFunc("refund", "refund the order", doRefund,
	agent.WithApproval(&agent.ApprovalPolicy{
		Need:             2,
		Approvers:        []string{"ops", "finance", "risk"},
		ExcludePrincipal: true, // the run's requester may not approve
	}))
```

अनुरोधकर्ता वह `OnBehalfOf` है जो उस `agent.Identity` में है जिससे डिप्लॉयमेंट रन शुरू करता है; ऊपर के रनों के प्रिंसिपल भी गिने जाते हैं। बाहर रखा गया निर्णय चिह्नित होकर रिकॉर्ड में रहता है। Bide id की तुलना करता है; यह नहीं देख सकता कि एक व्यक्ति के पास दो id हैं। देखें [कर्तव्यों का पृथक्करण](../../docs/guides/hitl-approval.md#separation-of-duties)।

**समय-सीमा**: नीति में `Timeout` दें (प्लान कॉन्फ़िग के `approval` ब्लॉक में `timeout: 48h`); तब तक पास न हुआ गेट हमेशा प्रतीक्षा करने के बजाय समाप्त हो जाता है। समय-सीमा दर्ज किए गए अनुरोध से गिनी जाती है और एक ही बार दर्ज होती है, इसलिए पुनः-डिप्लॉय उसे नहीं खिसकाता। समाप्ति गेट का परिणाम है और पास की तरह एक ही बार दर्ज होती है, इसलिए जो पहले दर्ज हुआ वही रहता है: कॉल कभी नहीं चलती (मॉडल `agent.ExpiredResult` पढ़ता है; `plan` फ़्लो `plan.ErrApprovalExpired` लौटाता है), और समय-सीमा के बाद दर्ज निर्णय "गिना नहीं गया" दिखता है और अस्वीकार होता है (`agent.ErrApprovalExpired`)। नोटिफ़ायर `approval_expired` भेजता है। देखें [समय-सीमा](../../docs/guides/hitl-approval.md#deadlines)।

**टर्मिनल से निर्णय**: जो अनुमोदक Go नहीं लिखता वह `bide approve` का उपयोग करता है। यह जर्नल से कॉल पढ़ता है, उसके तर्क और वह SHA-256 दिखाता है जिससे हस्ताक्षर बँधा है, पुष्टि माँगता है, अनुमोदक की कुंजी से हस्ताक्षर करता है, निर्णय दर्ज करता है और एक रसीद लिखता है:

```
bide approve -db run.db -list                  # the calls that wait, each with its command
bide approve -db run.db -run refund-1049 -call c2 -as finance -key finance.key \
    -approver-keys approver-keys.json
```

जो तर्क दिखाने के लिए बहुत लंबे हैं उन पर यह `-full` के बिना हस्ताक्षर नहीं करता, और निर्णय न गिना जाए तो कुछ दर्ज नहीं करता। देखें [टर्मिनल से निर्णय](../../docs/guides/hitl-approval.md#deciding-from-the-terminal)।

**लोगों को बताना**: जो रन अज्ञात परिणाम पर रुकता है या अनुमोदन गेट पर प्रतीक्षा करता है, वह एक सूचना उठाता है (`agent.WithNotifier`, हस्ताक्षरित वेबहुक `notify.NewWebhook` से पहुँचाई जाती है)। `agent.WithNotifyRoute` हर प्रकार की घटना एक प्राप्तकर्ता को भेजता है (`WithNotifyApproverRoute` अनुमोदन को अनुमोदक के आधार पर रूट करता है), और जब घटना की समय-सीमा तक पावती नहीं मिलती (अनुमोदन: निर्णय नहीं हुआ) तब `agent.WithNotifyEscalation` अगले प्राप्तकर्ता को सूचित करता है, अधिकतम चार चरणों की शृंखला में। पावती का अर्थ है कि एंडपॉइंट ने 2xx लौटाया, यह नहीं कि किसी व्यक्ति ने उसे पढ़ा। देखें [रूटिंग और एस्केलेशन](../../docs/guides/notifications.md#routing-and-escalation)।

## त्रुटियाँ

विफलताएँ प्रहरी त्रुटियों (sentinel errors) से वर्गीकृत होती हैं जिन्हें `errors.Is` मिलाता है। दो स्तर: एक **श्रेणी (category)** (मोटा वर्ग) और एक **स्थिति (condition)** (एक विशिष्ट कारण) जो अपनी श्रेणी को लपेटती है, ताकि एक मिलान जिस भी स्तर पर आपको ज़रूरत हो वहाँ काम करे:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string; func backOffAndRetry(); func fixToolWiring(); func alertOps() -->
```go
_, err := a.Run(ctx, runID, agent.UserText(input))
switch {
case errors.Is(err, agent.ErrModel):       // any provider fault (HTTP status, decode, stream)
	backOffAndRetry()
case errors.Is(err, agent.ErrUnknownTool):  // a specific condition (implies agent.ErrTool)
	fixToolWiring()
case errors.Is(err, agent.ErrStorage):      // durable-store I/O
	alertOps()
}
```

श्रेणियाँ: `ErrConfig`, `ErrModel`, `ErrTool`, `ErrStorage`, `ErrProtocol`, `ErrBudget`। स्थितियाँ (हर एक एक श्रेणी को लपेटती है): `ErrUnknownTool`, `ErrToolArgs` (`ErrTool` को लपेटती हैं), `ErrToolReinvoked`, `ErrInvalidApproval`, `ErrAlreadyDecided` (`ErrConfig` को लपेटती हैं), `ErrNoRecordedOutput`, `ErrIncompleteResponse` (`ErrModel` को लपेटती हैं), `ErrTruncatedToolArgs` (`ErrProtocol` को लपेटती है), `ErrBudgetExceeded`, `ErrMaxTurns` (`ErrBudget` को लपेटती हैं)। `ErrToolNotCalled` किसी श्रेणी को नहीं लपेटती: यह एक ऐसी टूल कॉल को चिह्नित करती है जिसके बारे में ज्ञात है कि वह अपने टूल तक कभी नहीं पहुँची (एक टूल middleware की अस्वीकृति इसे लपेटती है)। प्रदाता अडैप्टर `*RateLimited` (HTTP 429, एक `RetryAfter` संकेत के साथ) और `*APIError` (अन्य non-2xx, `StatusCode` के साथ) भी लौटाते हैं, दोनों `ErrModel` को लपेटते हैं। टूलकिट जो भी त्रुटि लौटाता है (मॉडल, MCP, स्टोर, और शासन अडैप्टरों से सहित) एक श्रेणी वहन करती है, इसलिए `errors.Is` पूरी सतह पर विश्वसनीय है।

और **नियंत्रण-प्रवाह संकेत** एक श्रेणी से समृद्धतर हैं, इसलिए वे ठोस प्रकार बने रहते हैं जिन्हें `errors.As` मिलाता है: `*ApprovalPending` (अनुमोदन आवश्यक), `*InterruptPending` (इंसानी इनपुट की प्रतीक्षा), `*TimerPending` (टिकाऊ टाइमर लंबित), `*SignalPending` (एक बाहरी सिग्नल की प्रतीक्षा), `*OutcomeUnknown` (पुनरारंभ असुरक्षित), `*SagaAborted` (वापस लुढ़काया गया), और `*HaltTooYoung` (`ResolveHalt` से, जब `WithMinHaltAge` अभी बीता नहीं है)। ये सभी सील किए गए इंटरफ़ेस `agent.Pause` को संतुष्ट करते हैं; `agent.IsPause(err)` से जाँचें और `agent.AsPause(err)` से पढ़ें। एक रुका या ठहरा हुआ रन एक "विफलता" श्रेणी नहीं है; `RunID` / `ToolUseID` / क्षतिपूर्ति विवरण के लिए struct का निरीक्षण करें। रद्दीकरण सामान्य `context.Canceled` / `context.DeadlineExceeded` के रूप में उभरता है, और जो ड्राइव अपने रन की लीज़ (`agent.Lease`) खो जाने के कारण रद्द हुई, वह `ErrLeaseLost` के रूप में; रद्दीकरण की तरह, यह कोई श्रेणी वहन नहीं करती।

## Middleware और अवलोकनीयता

दो स्वतंत्र `func(Handler) Handler` शृंखलाएँ: एक मॉडल कॉल (`WithMiddleware`) के चारों ओर और एक हर टूल कॉल (`WithToolMiddleware`) के चारों ओर। पहले जोड़ा = सबसे बाहरी। दोनों *उत्परिवर्तनकारी और लघु-परिपथी* हैं: जो अंदर जाता है उसे दोबारा लिखें, जो बाहर आता है उसे रूपांतरित करें, या `next` को कॉल किए बिना लौटें।

<!-- docsnip: setup model agent.Model; journal *agent.Journal; tools []agent.Tool; import oteltrace "go.opentelemetry.io/otel/trace"; tracer oteltrace.Tracer -->
```go
var cost middleware.CostMeter
a, err := agent.New(model, journal,
	agent.WithTools(tools...),
	agent.WithTokenBudget(100_000), // per run, rebuilt from the journal on resume
	agent.WithMiddleware(
		middleware.Retry(3, middleware.WithBackoff(200*time.Millisecond, 10*time.Second)),
		middleware.Cost(&cost, middleware.Rates{InputPer1M: 3, OutputPer1M: 15}),
	),
	agent.WithToolMiddleware(middleware.ToolLog(log.Printf), middleware.ToolCache(), middleware.ToolRetry(3)),
)
if err != nil {
	panic(err)
}

// opt-in OTel gen_ai.* spans (provider and model from agent.ModelInfoOf); the core has no OTel dependency:
a, err = a.With(
	agent.WithMiddleware(trace.Model(tracer)),
	agent.WithToolMiddleware(trace.Tool(tracer)), // execute_tool span per call; nests across the sub-agent boundary
)
// ... after the run: cost.Snapshot() (answer and spend, in tokens and USD)
```

`Retry` जिटर के साथ घातांकीय बैकऑफ़ करता है और एक प्रदाता 429 पर एक `Retry-After` का सम्मान करता है (अडैप्टर एक टाइप-किया गया `*agent.RateLimited` लौटाता है); `Cost` टोकन उपयोग (कैश-रीड/राइट सहित) से USD को एक `CostMeter` में जमा करता है जिसे आप रन के बाद पढ़ते हैं।

चूँकि `trace.Tool` लूप के भीतर चलता है, उसका span टूल को सौंपे गए संदर्भ में बैठता है, इसलिए जब एक टूल स्वयं एक उप-एजेंट होता है, तो उप-एजेंट का रन और उसके अपने span बच्चों के रूप में नेस्ट होते हैं। ट्रेस उप-एजेंट सीमा को पार करता है।

टूल middleware टिकाऊ चरण के *भीतर* चलती है, इसलिए एक लघु-परिपथ परिणाम (एक `ToolCache` हिट) किसी भी टूल परिणाम की तरह जर्नल होता है, और वैसे ही एक नीति अस्वीकृति जो `agent.ErrToolNotCalled` को लपेटती है; एक अस्वीकृति को `agent.ErrToolNotCalled` को लपेटना ही चाहिए: जो नहीं लपेटती, वह एक साइड इफ़ेक्ट के परिणाम को अज्ञात छोड़ देती है, कुछ भी रिकॉर्ड नहीं होता, और रन रुक जाता है; पुनरारंभ इसे फिर से चलाता है और कभी middleware या टूल को दोबारा नहीं चलाता। `ToolRetry` और `ToolCache` केवल उन टूलों पर काम करते हैं जिनकी `Safety` इसकी अनुमति देती है (क्रमशः पुनः-प्रयास-सुरक्षित, और `ReadOnly`), और middleware चाहे जो करे, एजेंट एक ऐसे टूल को जो पुनः-प्रयास-सुरक्षित नहीं है, प्रति कॉल ज़्यादा-से-ज़्यादा एक बार चलाता है। `agent.ToolMiddleware` हस्ताक्षर के साथ अपनी लिखें:

<!-- docsnip: setup func authorized(context.Context, string) bool -->
```go
// Deny a tool by policy: the tool never executes; the model sees the error and reacts.
// A denial must wrap agent.ErrToolNotCalled: without it the agent cannot tell the tool did
// not run, so a side effect's outcome is unknown and the run halts.
func RequireTag(tag string) agent.ToolMiddleware {
	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, call agent.ToolCall) (json.RawMessage, error) {
			if !authorized(ctx, tag) {
				return nil, fmt.Errorf("tool %q denied: %w", call.Use.Name, agent.ErrToolNotCalled)
			}
			return next(ctx, call) // mutate call.Use.Args before, transform the result after
		}
	}
}
```

## मॉड्यूल

Bide एक बहु-मॉड्यूल रेपो है: एक निर्भरता-हल्का **कोर** (`github.com/bide-ai/bide`, यानी लूप, schema, middleware, मॉडल अडैप्टर, `plan` फ़्लो बिल्डर, `audit`; मानक लाइब्रेरी के बाहर कोई निर्भरता नहीं) साथ ही प्रति भारी अडैप्टर एक मॉड्यूल (`mcptools`, `trace`, `store/sqlite`, `store/postgres`, `govern/redislog`, `govern/sqlitelog`, `govern/postgreslog`, `codec/gcf`), `bide` कमांड (`cmd/bide`) और `govern` मॉड्यूल, जो gsm को वहन करता है और gsm के स्थिर होने तक v0.x पर रहता है। एक अडैप्टर import करें और आप उसका निर्भरता वृक्ष खींच लेते हैं; केवल कोर import करें और आप नहीं खींचते। एक केवल-कोर उपभोक्ता की बाह्य-मॉड्यूल सतह 0 है, 54 नहीं। देखें [docs/reference/module-structure.md](../../docs/reference/module-structure.md)।

## आर्किटेक्चर

षट्कोणीय (hexagonal): कोर पोर्ट परिभाषित करता है (`Model`, `Store`, `Tool`, `Middleware`); अडैप्टर किनारों पर प्लग होते हैं। निर्भरताएँ अंदर की ओर इंगित करती हैं; कोर किसी अडैप्टर और किसी अवसंरचना को import नहीं करता, जिसकी रक्षा `architecture_test.go` करता है।

```
agent (root)     durable loop · Journal/Store · Message/Part · Tool/Safety · middleware types · RenderMermaid
plan             optional typed flow builder + declarative config; lowers to the loop (Topology · Conform)
model/anthropic  native Claude (thinking + signatures)
model/openai     any OpenAI-compatible endpoint
model/gemini     native Gemini (generativelanguage / Vertex via WithBaseURL)
schema           reflect Go types → inline JSON Schema + OpenAIStrict
middleware       Retry, RateLimit, Cost, Hedge
trace            opt-in OTel gen_ai.* spans
store/sqlite     on-disk durable resume (single binary, no cluster)
store/postgres   HA durable resume (any node resumes any run)
govern           Tier-2: federated governed state + quorum for agents that must agree (gsm-backed; own module)
```

## संघीय शासन (Tier-2)

टिकाऊ कोर एक एजेंट के काम को क्रैश-सुरक्षित रखता है। `govern` स्तर उन अनेक स्वतंत्र रूप से चलाए गए एजेंटों के लिए है जिन्हें सहमत होना ही है, प्रोसेस, टीम, या संगठनात्मक सीमाओं के आर-पार, साझा, पूर्णतः क्रमित इवेंट लॉग के अलावा किसी समन्वयक के बिना, और बिना किसी एकल लेखक के। यह सहमति के दो सत्यापनीय रूप देता है; दोनों में, एक पक्ष सार्वजनिक कलाकृतियों से परिणाम जाँचता है, किसी और के एजेंट पर भरोसा किए बिना।

**साझा अवस्था पर सहमति (अभिसरण)।** साझा अवस्था को एक रजिस्ट्री के रूप में वर्णित करें (चर + अपरिवर्तनीय + घटनाएँ); gsm *निर्माण समय पर* जाँचता है कि एजेंट क्रियाओं का हर अंतर्वयन उसी वैध अवस्था पर पहुँचता है, या निर्माण से इनकार करके आपको एक प्रति-उदाहरण दिखाता है (इसका दायरा देखें [ज्ञात सीमाएँ](../../docs/KNOWN-LIMITATIONS.md#governed-state-gsm) में)। रनटाइम O(1) तालिका लुकअप है; अवस्था घटना-स्रोतित और क्रैश-पुनर्प्राप्य है। यह एक एकल साझा रजिस्ट्री से ऊपर की ओर **संघों (federations)** के आर-पार बढ़ता है (सीमा-पार बाधाएँ: वृक्ष, समाधानकर्ताओं के साथ बहु-स्रोत DAG, एकदिष्ट चक्रीय *जाल (meshes)*), `Embed` के माध्यम से संघटित होता है, और आपके लिए क्षतिपूर्ति को संश्लेषित कर सकता है (नियम घोषित करें, एक अभिसारी शासक पाएँ, या एक प्रमाण कि कोई मौजूद नहीं)। एजेंट `FederatedEventTool` के माध्यम से प्लग होते हैं, ताकि एक LLM टूल कॉल एक शासित घटना बन जाए।

**एक निर्णय पर सहमति (कोरम)।** k-of-n नामित मतदाता (हर एक एक मॉडल, प्रदाता, या प्रधान) एक सामान्यीकृत निर्णय डालते हैं; हर मत एक जर्नल-किया गया, ज़्यादा-से-ज़्यादा-एक-बार वाला चरण है जो रिकॉर्ड करता है कि किसने कैसे मत दिया, और k-of-n द्वार मत-गणना पर एक gsm अपरिवर्तनीय है, ताकि "k सहमत हुए" हर संभव गणना पर मशीन-जाँचा जाए। `bide audit verify-quorum` सार्वजनिक कलाकृतियों से गणना और हर मत को पुनः-जाँचता है, उत्पादक पर भरोसा किए बिना बहुलता नियम को पुनरुत्पादित करता है। दावा सटीक है: एक कोरम सिद्ध करता है *कि k मतदाता सहमत हुए* और एकल-मॉडल जोखिम कम करता है; यह प्रमाणित नहीं करता कि निर्णय सही है (सहसंबंधित त्रुटियाँ स्वतंत्रता नहीं हैं), और केवल सामान्यीकृत निर्णयों को ही कोरम-किया जा सकता है, मुक्त-रूप गद्य को नहीं।

<!-- docsnip: setup ctx context.Context; machine *gsm.Machine; import "github.com/blackwell-systems/gsm"; log govern.EventLog -->
```go
gov, _ := govern.NewPersistent(ctx, machine, log, "order-42", machine.NewState())
tool := govern.EventTool(gov, govern.EventToolConfig{Name: "pay", Description: "mark the order paid", Event: "pay"})
// hand `tool` to the agent: concurrent agents sharing `gov` converge, durably.
```

> पूरी गाइड, क्षमता सीढ़ी, और चलाने योग्य डेमो (`examples/govern/mesh`, `examples/govern/compose`, `examples/govern/quorum`) **[docs/guides/governance.md](../../docs/guides/governance.md)** में।

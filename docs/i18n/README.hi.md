[English](../../README.md) · [简体中文](README.zh-CN.md) · [Русский](README.ru.md) · **हिन्दी** · [العربية](README.ar.md)

<p align="center">
  <img src="../../assets/bide-banner.png" alt="Bide">
</p>

**Go में टिकाऊ (durable) AI एजेंट बनाएँ। साइड इफ़ेक्ट जो ज़्यादा-से-ज़्यादा एक बार चलते हैं।**

*एक विराम जिसे आप हटा सकते हैं, उस दोहरे फ़ायर से बेहतर है जिसे आप पलट नहीं सकते।*

एक ही append-only जर्नल, और चार गारंटियाँ जिन्हें कोई दूसरा एजेंट फ़्रेमवर्क एक ही लाइब्रेरी में जोड़कर नहीं देता: साइड इफ़ेक्ट जो **ज़्यादा-से-ज़्यादा एक बार** चलते हैं; हज़ारों समवर्ती (concurrent) टिकाऊ रन **एक ही प्रोसेस में, बिना किसी क्लस्टर के**; एक **क्रिप्टोग्राफ़िक रूप से सत्यापनीय ऑडिट ट्रेल** (RFC 6962 Merkle प्रमाण, जिन्हें विक्रेता पर भरोसा किए बिना जाँचा जा सकता है); और **प्रमाणनीय रूप से अभिसारी (provably convergent)** साझा अवस्था। आपको ये चारों एक ही तंत्र से मिलते हैं, न कि चार एकीकृत सिस्टमों से, एक सादी Go लाइब्रेरी के रूप में। उन एजेंटों के लिए बनाया गया जो पैसा हिलाते हैं, रिकॉर्ड छूते हैं, या ऑडिट के तहत काम करते हैं।

**एंबिएंट एजेंटों के लिए बनाया गया।** एक एंबिएंट एजेंट बिना निगरानी के चलता है: यह तब तक सोता है जब तक कोई ट्रिगर (एक शेड्यूल या एक घटना) इसे न जगा दे, घंटों या दिनों तक काम करता है, और केवल तभी रुककर किसी इंसान से पूछता है जब उसे निर्णय की ज़रूरत होती है, जबकि हर कदम पर कोई नहीं देख रहा होता। ठीक तभी ज़्यादा-से-ज़्यादा-एक-बार, HA पुनरारंभ (resume), और एक सत्यापनीय ट्रेल केवल अच्छे-होते-तो-अच्छा-था वाली चीज़ें नहीं रह जातीं; एक पृष्ठभूमि एजेंट जो अनदेखा रहते हुए काम करता है, उसे क्रैश होने पर सुरक्षित, दोबारा ट्रिगर होने पर सुरक्षित, और तथ्य के बाद प्रमाणनीय होना ही होगा। Bide इसके लिए टिकाऊ जीवनचक्र देता है: टिकाऊ `Sleep`/`WaitUntil` टाइमर, समय- या घटना-चालित जगाने के लिए एक प्लग-करने-योग्य `Waker`, और टाइप-किए गए human-in-the-loop के लिए टिकाऊ `Interrupt`/`AnswerInterrupt`, सब कुछ एक ही जर्नल पर। आप ट्रिगर स्रोत और निगरानी UI लाते हैं; रनटाइम हर रन को नींद, क्रैश और नोड-हस्तांतरण के आर-पार सही बनाए रखता है।

स्थिति: **कार्यरत v0**, एंड-टू-एंड लाइव-सत्यापित। **Go 1.27** की आवश्यकता है।

## एक जर्नल, चार गारंटियाँ

हर कोई एक एजेंट लूप देता है; हमारा ~40 लाइनों का है। जो मायने रखता है वह इसके नीचे का आधार है: एक टिकाऊ, append-only जर्नल जिससे चारों गारंटियाँ *व्युत्पन्न* होती हैं, ताकि आपको वे चार सिस्टमों को एकीकृत करने के बजाय एक ही तंत्र से मिलें।

### 1 · ज़्यादा-से-ज़्यादा एक बार, न कि कम-से-कम एक बार (मापा गया, दावा किया गया नहीं)

Temporal, DBOS, trpc-agent-go, ADK, eino सभी **दोबारा चलाकर** पुनरारंभ होते हैं: गतिविधियों/चरणों का idempotent होना ज़रूरी है, इसलिए एक non-idempotent साइड इफ़ेक्ट (एक चार्ज, एक ईमेल, एक शिपमेंट) एक क्रैश के आर-पार दो बार चल सकता है। हमने एक **निष्पक्ष** क्रैश-इंजेक्शन बेंचमार्क बनाया ([`chaos/`](../../chaos), क्रॉस-SDK परिणाम [`benchmarks/`](../../benchmarks/README.md) में) जो एक non-idempotent `charge` को हर क्रैश-बिंदु से गुज़ारता है। यह संख्या *ही* उत्पाद है:

```
Bide      maxFired=1    ✓ at-most-once held
trpc-agent-go  maxFired=6    ✗ double-charged
adk-go         maxFired=4    ✗
langchaingo    maxFired=64   ✗
eino           maxFired=64   ✗
```

`maxFired` वह अधिकतम बार है जितनी बार एक साइड इफ़ेक्ट वास्तव में निष्पादित हुआ। **1 सही है; इससे ज़्यादा एक डबल-चार्ज है।** प्रतिस्पर्धी अडैप्टर सत्यापित रूप से स्ट्रॉमैन *नहीं* हैं (हर एक के पास एक निष्पक्षता परीक्षण है जो साबित करता है कि उसका पुनरारंभ सचमुच काम करता है)। जो चीज़ उनमें से किसी के पास नहीं: एक non-idempotent राइट से पहले लिखा गया एक टिकाऊ **प्रयास चिह्न (attempt marker)**, और पुनरारंभ पर **अज्ञात-परिणाम-पर-रुक जाना (halt-on-unknown-outcome)**: यदि किसी राइट का परिणाम कभी जर्नल नहीं हुआ, तो रन अनुमान लगाने के बजाय किसी इंसान के निर्णय के लिए रुक जाता है।

### जब परिणाम अज्ञात हो, तो यह रुक जाता है

ज़्यादा-से-ज़्यादा-एक-बार में कठिन मामला वह क्रैश नहीं जिसे आप देख सकते हैं, बल्कि वह है जिसे आप नहीं देख सकते: एक साइड इफ़ेक्ट जिसकी कॉल प्रोसेस से बाहर जा चुकी है पर जिसका परिणाम कभी जर्नल तक नहीं पहुँचा। प्रयास चिह्न एक पुनरारंभ हुए रन को "कभी शुरू नहीं हुआ" और "शुरू हुआ, परिणाम अज्ञात" में अंतर करने देता है, और अज्ञात मामले को एक निश्चित पदानुक्रम से हल करता है, कभी अनुमान से नहीं:

<p align="center">
  <img src="../../assets/resolution-ladder.png" width="820" alt="अज्ञात-परिणाम समाधान सीढ़ी: एक पुनः-प्रयास-सुरक्षित इफ़ेक्ट स्वतः पुनः-प्रयास करता है और प्रदाता दोहराव हटाता है; जो इफ़ेक्ट एक क्वेरी-योग्य रिकॉर्ड छोड़ गया, उसे एक reconciler स्वचालित रूप से हल करता है; एक वास्तव में अज्ञेय परिणाम रुकता है और एक इंसान की प्रतीक्षा करता है। अंतिम अस्पष्टता में, यह रुक जाता है।">
</p>

कोई टूल किस स्तर पर आता है, यह उसकी घोषित `Safety` तय करती है: उसे read-only, idempotent चिह्नित करें, या उसे एक idempotency key दें, और एक अज्ञात परिणाम स्वतः पुनः-प्रयास होता है; इनमें से कुछ भी घोषित न करें और वह रुक जाता है। पुनः-प्रयास-सुरक्षा opt-in है; जब आपने opt-in नहीं किया तब विराम डिफ़ॉल्ट है, ताकि एक लाइब्रेरी जिसका पूरा उद्देश्य "कभी दो बार फ़ायर न करना" है, अनुमान लगाने के बजाय सुरक्षित पर डिफ़ॉल्ट करे।

अधिकांश अज्ञात कभी किसी व्यक्ति तक नहीं पहुँचते: एक idempotency key प्रदाता को एक सुरक्षित पुनः-प्रयास का दोहराव हटाने देती है, और जिन सिस्टमों में वह नहीं होती (ईमेल, आंतरिक सेवाएँ) उनके लिए एक reconciler चरण को उस रिकॉर्ड से हल करता है जो वह छोड़ गया (`agent.ResolveHaltRef`)। इंसान न्यूनतम आधार है, डिफ़ॉल्ट नहीं।

> [!IMPORTANT]
> **इसके नीचे का नियम:** जब कोई क्रिया पैसा हिलाती है, किसी रिकॉर्ड को छूती है, या ऑडिट के तहत होती है, और परिणाम
> वास्तव में अज्ञेय है, तो रुक जाना ही सही परिणाम है। एक विराम जिसे एक इंसान या एक reconciler हटा सकता है, उस
> डबल-चार्ज से बेहतर है जिसे कोई वापस नहीं ले सकता।

### 2 · टिकाऊ निष्पादन एक लाइब्रेरी के रूप में, न कि एक क्लस्टर के रूप में

Temporal के पास गारंटियाँ हैं पर चलने के लिए एक सर्वर + एक वर्कर फ़्लीट चाहिए। यहाँ वे एक **ऐसे स्टोर अडैप्टर से आती हैं जिसे आप पहले से चला रहे हैं** (स्थानीय रूप से SQLite, प्रोडक्शन में Postgres)। एक hello-world केवल **स्टैंडर्ड लाइब्रेरी** को import करता है: कोई Temporal नहीं, कोई gRPC नहीं, कोई वेक्टर DB आपके बाइनरी में घसीटी नहीं जाती (`architecture_test.go` द्वारा प्रवर्तित)। इसे import करें; इसे चलाएँ (operate) नहीं।

और चूँकि यह एक Go लाइब्रेरी है, एक ही प्रोसेस बहुत बड़ी संख्या में इन टिकाऊ रनों को एक साथ उड़ान में रखता है। एजेंट का काम I/O-बाउंड होता है (मॉडल और टूल कॉलों पर प्रतीक्षा), जिसे goroutines बिना किसी क्लस्टर के सोख लेती हैं। [`cmd/bench`](../../cmd/bench/README.md) हार्नेस इसे मापता है: 20,000 रन, एक समय में 5,000 उड़ान में, हर एक मॉडल पर ~100ms ब्लॉक होता है, कुछ हज़ार goroutines और दसियों MB पर **10-कोर Apple silicon Mac पर लगभग आधे सेकंड (~470ms, v0.7.0 पर मापा गया) और एक मानक 4-vCPU CI रनर पर लगभग एक सेकंड की वॉल-क्लॉक** में पूरे हो जाते हैं (`go run ./cmd/bench -runs 20000 -concurrency 5000 -latency 50ms`)। लाभ थ्रूपुट और परिचालन सरलता में है, न कि मॉडल से कम लेटेंसी में (प्रति-कॉल लेटेंसी प्रदाता के हाथ में है); उच्च फ़ैन-आउट पर टिकाऊ स्टोर का राइट थ्रूपुट सीमा है, न कि goroutines। हर समवर्ती रन चारों गारंटियाँ रखता है। उस लोड के तहत विश्वसनीयता अंतर्निहित है: प्रति-प्रयास **टाइमआउट**, बैकऑफ़ के साथ पुनः-प्रयास जो क्षणिक बनाम टर्मिनल त्रुटियों को **वर्गीकृत** करता है, **हेज्ड (hedged)** मॉडल कॉल (एक बैकअप के साथ दौड़, पहला लो, टेल-लेटेंसी और प्रदाता फ़ेलओवर के लिए), और मॉडल तथा टूल कॉलों के लिए एक **रेट लिमिटर** ([middleware](../../middleware), [docs/guides/reliability.md](../../docs/guides/reliability.md))।

उच्च उपलब्धता के लिए, कोई भी नोड साझा स्टोर से किसी भी रन को पुनरारंभ करता है, और प्रतिस्पर्धी ड्राइवर प्रति-रन एक **लीज़ (lease)** (`agent.Lease`) के माध्यम से समन्वय करते हैं: सामान्यतः एक समय में एक ही प्रोसेस एक रन को चलाता है, और एक क्रैश हुए धारक की लीज़ समाप्त हो जाती है ताकि दूसरे नोड का `agent.RecoverLoop` उसे संभाल ले। जो धारक अपनी लीज़ की अवधि से अधिक समय तक रुका रहे, वह जागने पर भी रन चलाता हुआ मिल सकता है, पर वह किसी साइड इफ़ेक्ट को दूसरी बार नहीं चला सकता: ज़्यादा-से-ज़्यादा-एक-बार प्रयास दावे (attempt claim) पर टिका है, लीज़ पर नहीं। गारंटी 1 की तरह, यह सत्यापित है, अभिकथित नहीं: इन-मेमोरी स्टोर पर समवर्ती-वर्कर पारस्परिक अपवर्जन, क्रैश-और-अधिग्रहण, और समवर्ती ड्राइवरों के तहत ज़्यादा-से-ज़्यादा-एक-बार (`agent/ha_e2e_test.go`), और Postgres पर क्रॉस-प्रोसेस ज़्यादा-से-ज़्यादा-एक-बार, जहाँ दो स्टोर इंस्टेंस एक ही डेटाबेस साझा करते हैं (`store/postgres/postgres_test.go` में `TestPostgres_HAAtMostOnceAcrossInstances`; Postgres बैकएंड लीज़ को एक DB-घड़ी upsert से लागू करता है)।

### 3 · उसी जर्नल से एक क्रिप्टोग्राफ़िक रूप से सत्यापनीय ऑडिट रीढ़

<p align="center"><img src="../../assets/merkle.png" width="820" alt="Merkle समावेशन प्रमाण: एक जर्नल रिकॉर्ड (charge) अपने सहोदर पथ से होते हुए हस्ताक्षरित रूट तक हैश होता है, यह सिद्ध करते हुए कि रिकॉर्ड प्रतिबद्ध इतिहास में है जबकि बाकी रिकॉर्ड छिपे रहते हैं।"></p>

वह जर्नल जो पुनरारंभ को सुरक्षित बनाता है *ही* ऑडिट रिकॉर्ड है, और यह उसी **क्रिप्टोग्राफ़ी से प्रतिबद्ध (committed) है जो Certificate Transparency इस्तेमाल करती है** ([RFC 6962](https://datatracker.ietf.org/doc/html/rfc6962), प्रकाशित संदर्भ वेक्टरों के विरुद्ध जाँचा गया)। एक विनियमित खरीदार के लिए जो अंतर मायने रखता है: यह **सत्यापनीय है, न कि केवल लॉग किया हुआ**। एक तीसरा पक्ष एक प्रमाण की जाँच करता है *आप पर, आपके डेटाबेस पर, या आपके लॉग पर भरोसा किए बिना*:

- **समावेशन प्रमाण (inclusion proof)**: सिद्ध करें कि एक विशिष्ट क्रिया घटी (यह चार्ज, यह अनुमोदन) O(log n) में, कोई अन्य रिकॉर्ड उजागर किए बिना (केवल उसकी स्थिति और रन का आकार)। एक ऑडिटर के लिए चयनात्मक प्रकटीकरण।
- **संगति प्रमाण (consistency proof)**: सिद्ध करें कि इतिहास में केवल जोड़ा गया, कभी दोबारा लिखा या पुनः-क्रमबद्ध नहीं किया गया।
- **हस्ताक्षरित ट्री हेड (signed tree head) + सतत एंकरिंग**: `AuditedStore` प्रति चरण एक प्रतिबद्धता पर हस्ताक्षर करता है और उसे बैंड-से-बाहर एक बाहरी ट्रांसपेरेंसी लॉग में प्रकाशित करता है; छेड़छाड़ केवल संदिग्ध नहीं, बल्कि प्रमाणनीय बन जाती है।
- **किसने कार्य किया, किस प्राधिकार के तहत**: वही पत्ती कार्य करने वाली पहचान से प्रतिबद्ध हो सकती है (कर्ता, किसकी ओर से, किस हस्ताक्षरित अनुदान के तहत) और प्रत्यायोजित प्राधिकार को एक शासित अपरिवर्तनीय (invariant) के रूप में प्रवर्तित कर सकती है, ताकि एक प्रमाण केवल यह न दिखाए कि क्या हुआ बल्कि यह भी कि उसके लिए कौन अधिकृत था। अपना खुद का IdP लाएँ; यह अधिकृत क्रिया को प्रमाणनीय बनाता है, यह प्रमाणीकरण (authentication) की जगह नहीं लेता।

**प्रमाण जिन्हें आप सत्यापित करते हैं, न कि लॉग जिन पर आप भरोसा करते हैं।** बाकी सब *अवलोकनीयता (observability)* प्रदान करते हैं (लॉग जिन पर आप भरोसा करते हैं क्योंकि विक्रेता SOC2 है); यह *एक क्रिप्टोग्राफ़िक प्रमाण है जिसे आप स्वयं जाँचते हैं*। एक क्रिया के लिए एक पोर्टेबल `ProofBundle` उत्पन्न करें (`audit.ProveToolCall`), या एक पूरे-रन का `EvidencePackage` (`audit.Evidence`) जो हर सारभूत क्रिया के प्रमाण को एक फ़ाइल में बंडल करता है, और इसे एक ऑडिटर को सौंप दें जो इसे `bide-audit verify` / `verify-evidence` से, या एक केवल-stdlib वाले सत्यापनकर्ता से जो कभी SDK import नहीं करता, ऑफ़लाइन सत्यापित करता है। **किसी दूसरे एजेंट फ़्रेमवर्क के पास यह बिल्कुल नहीं है।** वही रीढ़ जवाबदेही परत का बाकी हिस्सा भी वहन करती है, सब ऑफ़लाइन सत्यापनीय: प्रमाण-वाहक रन (एक `RunCertificate` जो पूरे रन की नीति अनुपालना का साक्ष्य देता है), क्षीणनकारी (attenuating) प्रत्यायोजन के साथ हस्ताक्षरित क्षमता अनुदान, एक साफ़ ऑडिट ट्रेल से अर्जित प्राधिकार, और शासित k-of-n कोरम। → [docs/guides/audit.md](../../docs/guides/audit.md)

### 4 · प्रमाणनीय रूप से अभिसारी साझा अवस्था (gsm)

शासित-अवस्था स्तर: अनेक प्रोसेस जो उसी टिकाऊ लॉग को फिर से चलाते हैं (replay) **समरूप अवस्था पर अभिसरित होते हैं**, जिसे एक **मशीन-जाँचे गए प्रमाण** का समर्थन है। **gsm** अभिसरण इंजन की सामान्यीकरण पुनर्लेखन प्रणाली संगमी (confluent) है, इसलिए वह क्रम जिसमें चरण फिर से चलते हैं, परिणाम को नहीं बदल सकता। प्रमाण स्वयंसिद्ध-मुक्त (axiom-free) है और Coq 8.18 तथा 8.20 पर CI-सत्यापित है (`Print Assumptions` "Closed under the global context" रिपोर्ट करता है): [Coq/Rocq प्रमाण](https://github.com/blackwell-systems/normalization-confluence/tree/main/coq) ([![verify](https://github.com/blackwell-systems/normalization-confluence/actions/workflows/verify.yml/badge.svg)](https://github.com/blackwell-systems/normalization-confluence/actions/workflows/verify.yml))। और प्रमाण केवल कोड के बगल में पड़ा नहीं रहता: gsm का अपना प्रति-मशीन फ़ैसला उस प्रमाण से निकाले गए **दो स्वतंत्र जाँचकर्ताओं द्वारा पुनः-प्रमाणित** होता है (एक उत्सर्जित चरण-सारणियों से अभिसरण की पुनर्गणना करता है, दूसरा सीधे नियमों से), ताकि gsm के Go सत्यापनकर्ता में एक बग किसी अनभिसारी मशीन को पास न होने दे। नियम अपारदर्शी क्लोज़र के बजाय **निरीक्षणीय संयोजक (combinator) डेटा** के रूप में व्यक्त हैं, और यही उन्हें क्रमबद्ध-करने-योग्य (serializable), पोर्टेबल, और पुनः-जाँचने-योग्य बनाता है; सत्यापन **फ़ुटप्रिंट-स्थानीय (footprint-local)** (`BuildCompositional`) रूप में भी चल सकता है ताकि उन मशीनों को प्रमाणित किया जा सके जिनका वैश्विक अवस्था-समष्टि गणना के लिए बहुत बड़ा है। इसी तरह स्वतंत्र एजेंट बिना किसी एकल लेखक के अवस्था साझा करते हैं। दावा सटीक है: *पुनर्खेल का क्रम-निरपेक्ष अभिसरण*, प्रमाणित, न कि "एजेंट हमेशा सहमत होते हैं"। संघीय परिणाम पूरी तरह यंत्रीकृत है, जिसमें असमकालिक (अराजक) क्रम-निरपेक्षता भी शामिल है।

पैमाने पर मूर्त: एक एकीकरण परीक्षण **1,00,00,000 (एक करोड़) तक शासित एजेंटों, एक समय में 2,048,** को *यादृच्छिक, अपरिवर्तनीय-उल्लंघनकारी* क्रमों से गुज़ारता है (हर रन एक सीमाबद्ध अपरिवर्तनीय का उल्लंघन करता है और उसकी क्षतिपूर्ति होती है), और अभिकथन करता है कि हर एजेंट उसी वैध सामान्य रूप पर अभिसरित होता है *और* एक ऑडिट प्रमाण उत्पन्न करता है जो ऑफ़लाइन सत्यापित होता है, एक ही प्रोसेस में एक सपाट ~3 MB जीवित ढेर (heap) के साथ (~13 मिनट, ~12.5 हज़ार एजेंट/सेकंड)। यह एक फ़्रेमवर्क-स्तरीय परीक्षण है (स्टब मॉडल, इन-मेमोरी स्टोर): यह शासन और ऑडिट यंत्रावली को पैमाने पर परखता है, न कि एक जीवंत LLM या एक प्रोडक्शन डेटाबेस को। देखें [docs/testing/testing.md](../../docs/testing/testing.md)।

### बनाम टिकाऊ-निष्पादन और एजेंट रनटाइम

| | **Bide** | Temporal / DBOS | ADK · eino · trpc · langchaingo |
|---|---|---|---|
| क्रैश पर non-idempotent साइड इफ़ेक्ट | **ज़्यादा-से-ज़्यादा एक बार (अज्ञात परिणाम पर रुकता है)** | कम-से-कम एक बार; गतिविधियाँ/चरण idempotent होने चाहिए | कम-से-कम एक बार; दोबारा चलता है (**मापा गया 4–64×**) |
| परिनियोजन | **एक लाइब्रेरी + एक DB जो आप पहले से चलाते हैं** | सर्वर + वर्कर फ़्लीट | लाइब्रेरी |
| छेड़छाड़-स्पष्ट ऑडिट | **RFC 6962 Merkle रीढ़ (वही जर्नल)** | अंतर्निहित नहीं | कोई नहीं |
| अभिसारी साझा अवस्था | **प्रमाणनीय (gsm)** | लागू नहीं | कोई नहीं |

### नीचे का शिल्प

चारों गारंटियों से परे, वे विवरण जो इस पर निर्माण को सुखद बनाते हैं:

- **डिफ़ॉल्ट रूप से सादा Go, एक वैकल्पिक टाइप-किए गए फ़्लो बिल्डर के साथ।** आप `if`/`for`/फ़ंक्शन लिखते हैं और ग्राफ़ एक *व्युत्पन्न* दृश्य है (`RenderMermaid`, `Topology`), न कि कुछ ऐसा जिसे लिखने के लिए आप मजबूर हैं। जब आप वास्तव में लिखी हुई टोपोलॉजी चाहते हैं, तो `plan` बिल्डर वह आपको देता है और उसी रनटाइम पर उतार देता है। देखें [Graphs](#graphs)।
- **Claude का तर्कण राउंड-ट्रिप में बच जाता है।** विस्तारित-चिंतन (extended-thinking) हस्ताक्षर संरक्षित रहते हैं; अधिकांश SDK उन्हें गिरा देते हैं, चुपचाप चिंतन + टूल उपयोग को तोड़ते हुए।
- **प्रदाता-सजग टूल स्कीमा।** एक परावर्तित स्कीमा, प्रति बोली उत्सर्जित (OpenAI सख्त मोड, आदि), न कि एक सामान्य स्कीमा जिसे सख्त मोड और Gemini अस्वीकार कर देते हैं।
- **कोई भी मॉडल, एक अडैप्टर।** नेटिव Claude, नेटिव Gemini, और कोई भी OpenAI-संगत एंडपॉइंट (OpenAI, Ollama, DeepSeek, Groq, OpenRouter, vLLM, Azure, xAI…) `WithBaseURL` के माध्यम से।
- **बहु-नोड फ़ेलओवर, समन्वित।** कोई भी नोड किसी भी रन को पुनरारंभ करता है (Postgres, कोई एकल-लेखक लॉक नहीं); एक प्रति-रन लीज़ प्रतिस्पर्धी पुनर्प्राप्तिकर्ताओं और जीवित वर्करों को दोहरे-ड्राइविंग से रोकती है, और एक क्रैश हुए धारक के रन लीज़ समाप्ति पर संभाल लिए जाते हैं।

## Graphs

अधिकांश एजेंट फ़्रेमवर्क ग्राफ़ को *आधार* बना देते हैं: वह चीज़ जिसे आपको लिखना ही है और वह चीज़ जो निष्पादित होती है, नोड, किनारों, एक अवस्था वस्तु, और कभी-कभी ऊपर एक दृश्य बिल्डर के साथ। Bide इसे उलट देता है। वही लेखन सतहें उपलब्ध हैं, एक दृश्य बिल्डर तक, पर उन परतों के रूप में जिन्हें आप एक सादे-Go जर्नल आधार के ऊपर चुनते हैं, कभी आधार के रूप में नहीं। कारण वैचारिक नहीं बल्कि सटीक है।

एक ग्राफ़ कोई अभिव्यंजक शक्ति नहीं जोड़ता। एक ग्राफ़ जो भी गणना करता है, सामान्य नियंत्रण-प्रवाह वही गणना करता है: एक अभिकलन ग्राफ़ एक नियंत्रण-प्रवाह ग्राफ़ ही है, और अनुक्रम, चयन, और पुनरावृत्ति उनमें से किसी को भी व्यक्त करने के लिए पर्याप्त हैं। कोई एजेंट व्यवहार ऐसा नहीं जिसे आप नोड-और-किनारे वाले ग्राफ़ के रूप में बना सकें पर `if`, `for`, और फ़ंक्शन से न लिख सकें। एक ग्राफ़ जो जोड़ता है वह क्षमता नहीं बल्कि *पुनर्वस्तुकरण (reification)* है: प्रवाह का एक प्रथम-श्रेणी प्रतिनिधित्व जिसे आप निरीक्षण, दृश्यांकन, स्थैतिक रूप से सत्यापन, और कोड के बाहर लिख सकते हैं। यह वास्तव में उपयोगी है, पर यह एक टूलिंग परत है, न कि एक आधार, और एजेंट बनाने के लिए इसकी आवश्यकता नहीं।

तो यहाँ आधार सादा Go है, और गारंटियाँ (टिकाऊपन, ज़्यादा-से-ज़्यादा-एक-बार, सत्यापनीय ट्रेल) जर्नल से आती हैं, न कि किसी ग्राफ़ से। ग्राफ़ फिर भी एक *व्युत्पन्न* दृश्य के रूप में मौजूद रहता है: `RenderMermaid` उसे उसी से पुनर्निर्मित करता है जो वास्तव में चला।

यदि आप लिखने के लिए एक ग्राफ़ चाहते हैं, वह परत पहले से मौजूद है: **`plan`** पैकेज एक सीमित, टाइप-जाँचा गया फ़्लो बिल्डर है जो इस रनटाइम तक संकलित होकर उतरता है और ज़्यादा-से-ज़्यादा-एक-बार तथा ऑडिट ट्रेल मुफ़्त में विरासत में लेता है। आप टाइप-किए गए नोडों (`Step`, `Tool`, `Model`, `Switch`, फ़ैन-इन `Join`, सीमाबद्ध `LoopBack`) को एक `Flow` में जोड़ते हैं, या उसी टोपोलॉजी को घोषणात्मक कॉन्फ़िग (`plan.Load`) के रूप में लिखते हैं जिसे एक उच्चतर परत जैसे कोई दृश्य बिल्डर उत्सर्जित कर सकती है। यह वह परत बनी रहती है जिसे आप चुनते हैं, न कि आधार: एक ग्राफ़-प्रथम फ़्रेमवर्क उल्टा नहीं दे सकता, क्योंकि उसके लिए ग्राफ़ एक विकल्प के बजाय आधार है।

एक जवाबदेही रनटाइम के लिए दिशा भी मायने रखती है। एक लिखा हुआ ग्राफ़ एक आरेख है जिस पर आप भरोसा करते हैं; एक व्युत्पन्न ग्राफ़ जर्नल से पुनर्निर्मित है, इसलिए यह ठीक वही है जो चला। `plan` परत दोनों को जोड़ती है: `Topology()` और `RenderMermaid()` घोषित आकार को उजागर करते हैं, और `Conform()` क्रिप्टोग्राफ़िक रूप से जाँचता है कि एक रन ने उस टोपोलॉजी का पालन किया जिसे उसने घोषित किया, वही सत्यापित-करो-भरोसा-मत-करो रुख जो बाकी सिस्टम का है। जो नियम किसी भी ऐसी परत को रनटाइम को फ़ोर्क करने से रोकता है वह यह है: एक नई सतह लिखने का एक तरीक़ा जोड़ सकती है, निष्पादन का कभी नहीं; हर परत उसी एक जर्नल-समर्थित रनटाइम तक उतरती है। देखें [docs/guides/flows.md](../../docs/guides/flows.md)।

### लिखने के तीन तरीक़े, एक रनटाइम

वही ऑर्डर-ट्राइएज फ़्लो, तीन तरीक़ों से। सादा Go डिफ़ॉल्ट है: सामान्य नियंत्रण-प्रवाह लिखें, और उन चरणों को नाम दें जिन्हें जर्नल को क्रैश-सुरक्षित बनाना है।

<!-- docsnip: setup ctx context.Context; store agent.Durable; order Order; type Order struct{}; type Receipt struct{}; type Assessment struct{ Rush bool }; type Reservation struct{}; func classify(Order) (Assessment, error); func reserve(Assessment) (Reservation, error); func finalize(Reservation) (Receipt, error); func decline(Assessment) (Receipt, error) -->
```go
// classify, then branch: rush orders reserve-then-finalize, the rest decline.
assess, _ := agent.Step(ctx, store, "order-42", "classify",
    func(ctx context.Context) (Assessment, error) { return classify(order) },
    agent.StepSafety(agent.Safety{ReadOnly: true})) // safe to re-run after a crash

var receipt Receipt
if assess.Rush {
    res, _ := agent.Step(ctx, store, "order-42", "reserve", // a side effect: at most once
        func(ctx context.Context) (Reservation, error) { return reserve(assess) })
    receipt, _ = agent.Step(ctx, store, "order-42", "finalize",
        func(ctx context.Context) (Receipt, error) { return finalize(res) })
} else {
    receipt, _ = agent.Step(ctx, store, "order-42", "decline",
        func(ctx context.Context) (Receipt, error) { return decline(assess) })
}
```

जब आप उसी फ़्लो को एक प्रथम-श्रेणी, निरीक्षणीय कलाकृति के रूप में चाहते हैं, तो `plan` बिल्डर टाइप-किए गए नोडों को एक `Flow` में जोड़ता है जो उसी रनटाइम पर उतरता है:

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

तीनों उसी एक जर्नल-समर्थित रनटाइम तक उतरते हैं, इसलिए आप कोई भी सतह चुनें, ज़्यादा-से-ज़्यादा-एक-बार, HA पुनरारंभ, और सत्यापनीय ट्रेल मुफ़्त में मिलते हैं।

## एंबिएंट रन: टिकाऊ नींद, जागना, और अंतरायण

ऊपर की चारों गारंटियाँ आधार हैं; यह वह जीवनचक्र है जिसे वे संभव बनाती हैं। एक एंबिएंट रन एक समकालिक चैट लूप में नहीं बैठता। यह सोता है, एक ट्रिगर पर जागता है, और एक इंसान के लिए रुकता है, और उन संक्रमणों में से हर एक जर्नल पर एक टिकाऊ, ज़्यादा-से-ज़्यादा-एक-बार वाला चरण है, ताकि रन उनके बीच क्रैश और नोड-हस्तांतरण से बच जाए।

- **एक समय-सीमा तक सोना।** `Sleep`/`WaitUntil` एक रन को रोकते हैं और उसका जागने का समय जर्नल में लिखते हैं, ताकि विराम एक पुनरारंभ को जी ले। जागने के समय पर पुनः-आह्वान ठीक एक बार पुनरारंभ करता है।
- **समय या घटना पर जागना।** एक प्लग-करने-योग्य `Waker` (डिफ़ॉल्ट रूप से इन-प्रोसेस `MemWaker`) एक देय रन को पुनः-आह्वान करता है; ट्रिगर स्रोत आपका है (एक इन-प्रोसेस लूप, एक cron, एक क़तार, एक इनबाउंड webhook), ताकि वही आधार शेड्यूल-किए और घटना-चालित दोनों एजेंटों को चलाए।
- **एक इंसान के लिए, टिकाऊ रूप से अंतरायित करना।** `Interrupt[T]`/`AnswerInterrupt` एक रन को किसी भी बिंदु पर रोककर एक टाइप-किए गए निर्णय का अनुरोध करते हैं और इंसान के उत्तर के साथ एक जर्नल-किए चरण के रूप में पुनरारंभ करते हैं (देखें [Human-in-the-loop](#human-in-the-loop))। अनुमोदित/अस्वीकृत उस बूलियन का विशेष मामला है।

आप ट्रिगर स्रोत और निगरानी सतह देते हैं; रनटाइम रन को हर नींद, जागरण, अंतरायण, क्रैश, और हस्तांतरण के आर-पार सही बनाए रखता है। `examples/signals` (एक प्रतीक्षारत रन में एक घटना पहुँचाएँ), `examples/interrupt` (human-in-the-loop विराम/पुनरारंभ), और `examples/recover` (टिकाऊ पुनरारंभ) में चलाने योग्य। देखें [सिग्नल और एंबिएंट गाइड](../../docs/guides/signals.md)।

## गारंटी 1, कोड में: यह दो बार चार्ज नहीं करेगा

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string; type ChargeArgs struct{}; type Receipt struct{} -->
```go
// A tool that moves money is a write: not ReadOnly, not Idempotent.
charge := agent.Func("charge_card", "Charge the customer", agent.Safety{},
	func(ctx context.Context, in ChargeArgs) (Receipt, error) { /* ... */ })

// If the process crashes after the charge fires but before its result is journaled,
// resume does NOT run it again: it returns *OutcomeUnknown so you confirm, not double-charge:
_, err := a.Run(ctx, runID, input)
if halt, ok := errors.AsType[*agent.OutcomeUnknown](err); ok {
	// halt.Op.ToolName == "charge_card": outcome unknown, a human decides, no double side effect.
}
```

## त्वरित शुरुआत

Go 1.27 की आवश्यकता है (कोर जेनेरिक मेथड इस्तेमाल करता है)। यदि `go version` पुराना है, तो अपग्रेड करें या `GOTOOLCHAIN=go1.27.0` सेट करें।

कोर पैकेज `agent` है, जो `github.com/bide-ai/bide/agent` से import होता है (जैसा नीचे का ब्लॉक दिखाता है)।

```go
package main

import (
	"context"
	"fmt"
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
	weather := agent.Func("get_weather", "Current weather for a city",
		agent.Safety{ReadOnly: true},
		func(_ context.Context, in WeatherArgs) (Weather, error) {
			return Weather{TempF: 68, Sky: "sunny"}, nil
		})

	// Durable on-disk store: a crash mid-run resumes from here.
	store, _ := sqlite.Open("agent.db")
	defer store.Close()

	a := agent.New(model, store, weather).
		WithSystemPrompt("You are a concise weather assistant.")
	out, _ := a.Run(context.Background(), "run-1", "Weather in SF? Use the tool.")
	for _, p := range out.Parts {
		if t, ok := p.(agent.Text); ok {
			fmt.Println(t.Text)
		}
	}
}
```

लाइव स्मोक उदाहरण चलाएँ: `OPENROUTER_API_KEY=sk-... go run ./examples/smoke`

`Run` केवल अंतिम संदेश लौटाता है। एक रन सारांश (पूरे रन का टोकन उपयोग, कैश और सब-एजेंटों सहित; मॉडल-ट्रन गिनती; वॉल-क्लॉक अवधि) के लिए `RunResult` (और `RunSagaResult`) इस्तेमाल करें:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string -->
```go
res, err := a.RunResult(ctx, runID, input)
// res.Message, res.Usage, res.Spend, res.Turns, res.Duration, res.RunID
```

## स्ट्रीमिंग

`Run` ब्लॉक करता है और अंतिम उत्तर लौटाता है। एजेंट को काम करते देखने के लिए (टोकन डेल्टा, ट्रन सीमाएँ, टूल शुरू/समाप्त), `Stream` इस्तेमाल करें। यह **वही लूप** चलाता है (`Run` अक्षरशः `Stream(...).Final()` है), इसलिए टिकाऊपन, पुनरारंभ, और साइड-इफ़ेक्ट सुरक्षा समरूप हैं:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string -->
```go
stream := a.Stream(ctx, runID, input)
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
answer, err := stream.Final() // terminal message + error (incl. a Pause: *ApprovalPending, *OutcomeUnknown, ...)
```

घटनाएँ: `TurnStarted`, `ModelEvent` (टोकन फ़ीड), `AssistantTurn`, `ToolStarted` / `ToolCompleted`, `ApprovalRequired`, `Finished`। एक UI के लिए `Events()` पर रेंज करें फिर `Final()` को कॉल करें, या अकेले `Final()` कॉल करें ताकि यह ठीक `Run` की तरह व्यवहार करे (यह आपके लिए घटनाओं को निकाल देता है)।

दो बातें जानने योग्य, दोनों टिकाऊपन के परिणाम:
- **टोकन डेल्टा middleware शृंखला के नीचे पहुँचते हैं** (Retry / Cost फिर भी पूरे संयोजित संदेश देखते हैं), और **केवल एक ताज़ा मॉडल कॉल पर**।
- **पुनरारंभ पर, जर्नल-किया गया ट्रांसक्रिप्ट फिर से उत्सर्जित होता है** `AssistantTurn{Replayed: true}` + `ToolCompleted` के रूप में, लाइव प्रगति से पहले, ताकि एक ताज़ा UI क्रैश के बाद पूरी कहानी पुनर्निर्मित कर ले, और एक पुनर्खेल-किया गया ट्रन कोई टोकन डेल्टा उत्पन्न नहीं करता (वह पहले ही तय हो चुका था)।

`StreamSaga` `RunSaga` का स्ट्रीमिंग समकक्ष है।

## टाइप-किया गया आउटपुट

`RunTyped[T]` एक मुक्त-रूप संदेश के बजाय एक टाइप-किया गया `T` लौटाता है। यह एक कृत्रिम `final_answer` टूल इंजेक्ट करता है जिसका JSON स्कीमा `T` से व्युत्पन्न है (`schema` पैकेज के माध्यम से) और मॉडल को उसका काम पूरा होने पर इसे एक बार कॉल करने की ओर संचालित करता है, ताकि एक टूल-उपयोगी एजेंट वास्तविक काम कर सके और *फिर* टाइप-किया हुआ उत्तर दे। प्रदाता-अज्ञेय (नेटिव टूल कॉलिंग पर निर्मित, किसी प्रदाता के JSON मोड पर नहीं)।

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string -->
```go
type Weather struct {
	City  string `json:"city"`
	TempF int    `json:"temp_f"`
}

w, err := agent.RunTyped[Weather](ctx, a, runID, "weather in SF?")
// w.City == "SF", w.TempF == 68
```

यह एक पैकेज फ़ंक्शन है, मेथड नहीं (Go मेथड टाइप पैरामीटर नहीं जोड़ सकते)। मान *जर्नल-किए गए* टूल कॉल से डीकोड होता है, इसलिए यह **पुनरारंभ-सुरक्षित** है: एक रन के बीच का क्रैश पुनरारंभ पर लॉग से टाइप-किया गया उत्तर वापस पा लेता है। पहली `final_answer` कॉल जिसे टूल स्वीकार करता है, रन को समाप्त कर देती है। केवल यदि मॉडल ऐसी कोई कॉल कभी नहीं करता (वह इसके बजाय सादे JSON टेक्स्ट में उत्तर देता है), तभी `RunTyped` रन के अंतिम ट्रन के टेक्स्ट को पार्स करता है। `T` को एक JSON ऑब्जेक्ट होना चाहिए (एक struct, उसका पॉइंटर, या एक map), क्योंकि प्रदाता टूल आर्ग्युमेंट केवल ऑब्जेक्ट के रूप में लेते हैं; कोई भी अन्य `T` `ErrConfig` है।

सख्त संरचित आउटपुट वाले OpenAI-संगत प्रदाताओं पर, `RunTypedNative[T]` टूल के बजाय प्रदाता के नेटिव JSON-स्कीमा प्रतिक्रिया प्रारूप का उपयोग करता है (स्कीमा प्रदाता-पक्ष पर प्रवर्तित, कोई टूल राउंड-ट्रिप नहीं); Anthropic अडैप्टर इसका समर्थन नहीं करता और `ErrConfig` लौटाता है, इसलिए वहाँ प्रदाता-अज्ञेय आउटपुट के लिए `RunTyped` इस्तेमाल करें।

## सैंपलिंग

जनन नियंत्रण प्रदाता-निरपेक्ष हैं और एक बार सेट होते हैं; हर अडैप्टर उन्हें अपने वायर प्रारूप पर मैप करता है (और जो वह नहीं कर सकता उसे गिरा देता है, जैसे Anthropic के पास `seed` नहीं है):

<!-- docsnip: setup model agent.Model; store agent.Durable; tools []agent.Tool -->
```go
a := agent.New(model, store, tools...).
	WithSampling(agent.Temperature(0), agent.MaxTokens(500), agent.TopP(0.9), agent.Seed(42))
```

फ़ील्ड अभिकल्पना से वैकल्पिक हैं: एक अनसेट फ़ील्ड प्रदाता डिफ़ॉल्ट इस्तेमाल करता है, इसलिए एक स्पष्ट `Temperature(0)` "निर्दिष्ट नहीं" से भिन्न है। अनुरोध-स्तरीय `MaxTokens` एक अडैप्टर के निर्माण-समय डिफ़ॉल्ट को ओवरराइड करता है।

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
a1, _ := s.Send(ctx, "what's the capital of France?")
a2, _ := s.Send(ctx, "and its population?")   // sees turn 1 in context
```

ट्रांसक्रिप्ट सत्र id के तहत ट्रन-दर-ट्रन जर्नल होता है, ताकि एक पुनरारंभ हुआ प्रोसेस `a.Session(ctx, "user-42")` इसे पुनर्निर्मित करके जारी रखे। ट्रन N `"<id>>@turn/N"` के तहत चलता है (उसका अपना टिकाऊ जर्नल एक ट्रन के *भीतर* क्रैश-पुनरारंभ संभालता है); वार्तालाप स्मृति प्रश्न/उत्तर ट्रांसक्रिप्ट है: एक ट्रन के मध्यवर्ती टूल कॉल उस ट्रन में ही रहते हैं और बाद के ट्रनों में नहीं रिसते। यदि एक ट्रन रुकता है (अनुमोदन / `Interrupt`), तो `Send` वह त्रुटि लौटाता है; उसे हल करें और पुनरारंभ के लिए उसी इनपुट के साथ `Send` फिर कॉल करें। तब तक, किसी भिन्न संदेश के साथ `Send` `ErrConfig` लौटाता है: खुला ट्रन अपने संदेश का है। उन इनबाउंड संदेशों के लिए जो दोबारा पहुँचाए जा सकते हैं, `SendOnce(ctx, id, text)` हर संदेश id का उत्तर एक बार देता है। एक सत्र पर कई हैंडल कभी कोई ट्रन नहीं खोते, न ही एक संदेश का उत्तर किसी दूसरे के जवाब से देते हैं।

## ऑडिट-योग्यता (छेड़छाड़-स्पष्ट जर्नल)

टिकाऊ जर्नल पहले से एक रन के हर चरण को रिकॉर्ड करता है। `audit` पैकेज उस इतिहास से एक हैश शृंखला के साथ प्रतिबद्ध होता है, ताकि एक रन का निष्पादन सत्यापनीय हो:

<!-- docsnip: setup ctx context.Context; store agent.Durable; runID string; priv ed25519.PrivateKey -->
```go
head, _ := audit.Head(ctx, store, runID)     // SHA-256 chain over the journal (persisted order)
sig, _ := audit.Sign(head, audit.Ed25519Signer{Priv: priv}) // anchor it: sign / publish out-of-band
```

एक रिकॉर्ड का कोई भी संशोधन / प्रविष्टि / विलोपन / पुनः-क्रमण हेड को बदल देता है। **सुरक्षा मॉडल:** यह अखंडता बिना शर्त देता है, और छेड़छाड़-स्पष्टता *तब जब आप हेड को बैंड-से-बाहर एंकर करते हैं* (एक ही DB में एक शृंखला जिसे एक हमलावर नियंत्रित करता है, दोबारा लिखी और दोबारा हैश की जा सकती है); पैकेज दस्तावेज़ देखें। यह अनुपालना/उद्यम सीवन है: प्रमाणनीय ज़्यादा-से-ज़्यादा-एक-बार साइड इफ़ेक्ट *साथ ही* ठीक इस बात का सत्यापनीय रिकॉर्ड कि एजेंट ने क्या किया।

**चयनात्मक प्रकटीकरण** के लिए, `audit.Root` / `Prove` / `VerifyInclusion` एक **RFC 6962** (Certificate Transparency) Merkle ट्री बनाते हैं, ताकि आप एक O(log n) समावेशन प्रमाण के माध्यम से सिद्ध कर सकें कि एक रिकॉर्ड एक प्रतिबद्ध रन का भाग है, *बाकी रिकॉर्ड उजागर किए बिना* (जैसे एक ऑडिटर को दिखाएँ कि एक चार्ज हुआ, कोई अन्य ग्राहक या प्रॉम्प्ट प्रकट किए बिना)। और `ProveConsistency` / `VerifyConsistency` सिद्ध करते हैं कि एक पूर्ववर्ती रूट एक बाद वाले का **append-only उपसर्ग** है: कि इतिहास में केवल जोड़ा गया, कभी दोबारा लिखा या पुनः-क्रमबद्ध नहीं किया गया (ट्रांसपेरेंसी-लॉग गारंटी)। कार्यान्वयन प्रकाशित RFC 6962 परीक्षण वेक्टरों के विरुद्ध जाँचा गया है।

`SignTreeHead` CT-शैली का **हस्ताक्षरित ट्री हेड (Signed Tree Head)** उत्पन्न करता है, `{Kind, RunID, Size, Root, TimestampNanos}` जो अपनी हस्ताक्षर योजना सहित किसी भी `audit.Signer` (Ed25519, ML-DSA-65, या दोनों का हाइब्रिड) से हस्ताक्षरित है, वह कलाकृति जिसे आप प्रकाशित करते हैं। पूरा प्रवाह: एक STH पर हस्ताक्षर करें, बाद में एक समावेशन प्रमाण के साथ एक एकल रिकॉर्ड प्रकट करें जिसे एक ऑडिटर हस्ताक्षरित रूट के विरुद्ध जाँचता है, और दो STH के बीच append-only वृद्धि सिद्ध करें। मॉडल, API, और एंड-टू-एंड अनुपालना प्रवाह के लिए [docs/guides/audit.md](../../docs/guides/audit.md) देखें।

## RAG और स्मृति (अपनी लाएँ)

Bide **कोई वेक्टर स्टोर, एम्बेडर, या स्मृति बैकएंड नहीं** देता: यह आपको *सीवन* देता है और आप वह स्टोर प्लग करते हैं जिसे आप पहले से चलाते हैं। अपने अवसंरचना के विरुद्ध एक इंटरफ़ेस लागू करें:

<!-- docsnip: api agent -->
```go
type Retriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]agent.Doc, error)
}
```

फिर इसे दो में से एक तरीक़े से जोड़ें:

<!-- docsnip: setup model agent.Model; store agent.Durable; myStore agent.Retriever -->
```go
// Agentic RAG: the model searches on demand:
a := agent.New(model, store, agent.RetrievalTool(myStore, 5))

// Classic RAG: top-k auto-injected as context on each user turn:
a.Use(agent.WithRetrieval(myStore, 5))
```

वार्तालाप स्मृति पहले से अंतर्निहित है (`Session`); गतिशील संदर्भ `WithSystemPromptFunc` से गुज़रता है; यह सीवन शब्दार्थ / दीर्घकालिक स्मृति को समेटता है। मूर्त स्टोर अडैप्टर (यदि कभी आवश्यक हों) अलग मॉड्यूल होंगे, कभी कोर में नहीं। देखें [docs/guides/rag-memory.md](../../docs/guides/rag-memory.md)।

## पुनरारंभ सुरक्षा, एक तालिका में

```go
agent.Safety{ReadOnly: true}          // no side effects → always safe to re-run
agent.Safety{Idempotent: true}        // safe to retry (dedupes downstream)
agent.Safety{}                        // a write → HALT on unknown outcome, don't double-fire
agent.Safety{RequiresApproval: true}  // pause for human approval before executing
```

एक non-idempotent साइड इफ़ेक्ट से पहले लूप एक टिकाऊ *प्रयास चिह्न* रिकॉर्ड करता है, ताकि पुनरारंभ "कभी नहीं चला" (चलाना सुरक्षित) को "चला और क्रैश हुआ" (रुको) से बता सके: सटीकता से, न कि रूढ़िवादी रूप से।

यह **प्रमाणित है, अभिकथित नहीं।** `dst_test.go` एक नियतात्मक अनुकरण परीक्षण है: एक दोष-इंजेक्ट करने वाला स्टोर *हर* राइट-बिंदु पर क्रैश करता है (और सैकड़ों यादृच्छिकृत बहु-क्रैश शेड्यूलों के आर-पार), और हार्नेस अभिकथन करता है कि एक non-idempotent साइड इफ़ेक्ट हर बार **ज़्यादा-से-ज़्यादा एक बार** चलता है, और रन हमेशा completed या halted पर समाप्त होता है, कभी दो बार नहीं चलता।

हार्नेस निर्यातित है (`chaos/`) और `benchmarks/` में अन्य SDK पर इंगित है। मापा गया परिणाम: **Bide `maxFired=1` (PASS); trpc-agent-go `maxFired=6`; langchaingo `maxFired=64` (दोनों FAIL)।** trpc का चेकपॉइंट/पुनरारंभ सचमुच काम करता है (सत्यापित: एक पूर्ण रन को पुनरारंभ करना एक no-op है); उसका डबल-फ़ायर वह प्रलेखित LangGraph "नोड idempotent होने चाहिए" खिड़की है। langchaingo में टिकाऊपन बिल्कुल नहीं है, इसलिए पुनः-प्रयास सब कुछ दोबारा चलाते हैं। Bide का प्रयास-चिह्न उस खिड़की को पूरी तरह बंद कर देता है।

`WithMaxTurns(n)` प्रति रन मॉडल ट्रनों की एक ऊपरी सीमा लगाता है ताकि एक मॉडल जो टूल कॉल करता रहता है सदा के लिए लूप न कर सके: इस तक पहुँचना `ErrMaxTurns` लौटाता है (जो `errors.Is` `ErrBudget` है)। `WithTokenBudget(n)` उन टोकनों की सीमा लगाता है जो एक रन इस्तेमाल कर सकता है, कैश किया गया इनपुट सहित: एक बार रन `n` इस्तेमाल कर ले, तो वह कोई और मॉडल कॉल नहीं करता और `ErrBudgetExceeded` लौटाता है। हर कॉल का उपयोग उसके ट्रन के साथ जर्नल होता है, इसलिए दोनों सीमाएँ जर्नल से पुनर्निर्मित होती हैं और एक क्रैश और पुनरारंभ के आर-पार टिकी रहती हैं।

## Human-in-the-loop

तीन स्वाद। **अनुमोदन/अस्वीकृति**: `RequiresApproval` से चिह्नित एक टूल चलने से *पहले* रुकता है; इंसान का निर्णय एक bool है:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; store agent.Durable; runID string; input string -->
```go
_, err := a.Run(ctx, runID, input)
if pend, ok := errors.AsType[*agent.ApprovalPending](err); ok {
	// ... get a human decision ...
	agent.Approve(ctx, store, pend.RunID, pend.ToolUseID, true)
	out, _ := a.Run(ctx, pend.RootRunID, input) // resumes past the pause
}
```

**अंतरायण/पुनरारंभ**: एक टूल *एक मनमाने बिंदु पर* रुकता है और एक *टाइप-किए गए* मान के साथ पुनरारंभ होता है (bool का सामान्यीकरण)। एक पुनः-प्रयास-सुरक्षित टूल के भीतर `agent.Interrupt[T]` कॉल करें:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; store agent.Durable; runID string; input string; type Options struct{}; type Plan struct{}; chosenPlan Plan -->
```go
tool := agent.Func("choose_plan", "pick a plan", agent.Safety{ReadOnly: true},
	func(ctx context.Context, in Options) (Plan, error) {
		pick, err := agent.Interrupt[Plan](ctx, "plan", in) // pauses the run; in is shown to the human
		if err != nil {
			return Plan{}, err // *InterruptPending propagates out of Run
		}
		return pick, nil // on resume, pick is the human's typed answer
	})

_, err := a.Run(ctx, runID, input)
if intr, ok := errors.AsType[*agent.InterruptPending](err); ok {
	// ... show intr.Prompt, get a typed answer ...
	agent.AnswerInterrupt(ctx, store, intr.RunID, intr.Name, chosenPlan)
	out, _ := a.Run(ctx, intr.RootRunID, input) // resumes; Interrupt now returns chosenPlan
}
```

दोनों टिकाऊ हैं: निर्णय/मान एक जर्नल-किया गया चरण है, इसलिए यह एक क्रैश को जी लेता है। Interrupt को एक पुनः-प्रयास-सुरक्षित टूल में होना चाहिए (`ReadOnly`/`Idempotent`): पुनरारंभ पर टूल तब तक दोबारा चलता है जब तक अंतरायण हल न हो, इसलिए `Interrupt` कॉल से पहले सब कुछ दोहराने के लिए सुरक्षित होना चाहिए।

**m-of-n अनुमोदन**: जब एक हस्ताक्षर-स्वीकृति पर्याप्त नहीं, तो n अनुमोदकों के एक नामित समूह से k हस्ताक्षरित निर्णय आवश्यक करें। हर अनुमोदक ठीक उसी कॉल (टूल और तर्कों) पर हस्ताक्षर करता है; गेट k अनुमोदनों पर आगे बढ़ता है, k अप्राप्य होते ही अस्वीकार करता है, और अन्यथा चालू गणना के साथ रुकता है। एक जाली या ग़लत निर्णय अनदेखा किया जाता है, उसके अनुमोदक को बाहर किए बिना:

<!-- docsnip: setup ctx context.Context; model agent.Model; store agent.Durable; pend *agent.ApprovalPending; type RefundArgs struct{}; doRefund func(context.Context, RefundArgs) (string, error); keysByApprover agent.ApproverVerifierFor; signer audit.Signer -->
```go
refund := agent.Func("refund", "refund the order",
	agent.Safety{Approval: &agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}},
	doRefund)
a := agent.New(model, store, refund).WithApproverVerifiers(keysByApprover)

// each approver, out of band, signs the paused call they were shown:
sig, _ := signer.Sign(agent.ApprovalDecisionBytes(pend.Subject(), "finance", true))
agent.SubmitDecision(ctx, store, agent.Decision{RunID: pend.RunID, ToolUseID: pend.ToolUseID,
	ApproverID: "finance", Approved: true, Alg: signer.Alg(), Signature: sig})
```

फिर `audit.ApprovalEvidence` और `audit.VerifyApprovals` (या `bide-audit verify-approvals`) ऑफ़लाइन सिद्ध करते हैं कि k नामित अनुमोदकों ने ठीक इसी कॉल पर उसके चलने से *पहले*, अपेक्षित नीति के तहत, हस्ताक्षर-स्वीकृति दी, ऐसे साक्ष्य से जो किसी निर्णय को बिना पकड़े छोड़ नहीं सकता। हर अनुमोदक की अपनी कुंजी होनी चाहिए: जिस नीति के दो अनुमोदक एक ही कुंजी पर पहुँचते हैं, उसे `ErrConfig` के साथ अस्वीकार किया जाता है, क्योंकि उस कुंजी का धारक दोनों की ओर से हस्ताक्षर कर सकता है। देखें [अनुमोदन गाइड](../../docs/guides/hitl-approval.md); अलग-अलग प्रोसेसों के आर-पार `examples/approval` में चलाने योग्य।

## त्रुटियाँ

विफलताएँ प्रहरी त्रुटियों (sentinel errors) से वर्गीकृत होती हैं जिन्हें `errors.Is` मिलाता है, स्टैंडर्ड-लाइब्रेरी मुहावरा, कोई कस्टम त्रुटि फ़्रेमवर्क नहीं। दो स्तर: एक **श्रेणी (category)** (मोटा वर्ग) और एक **स्थिति (condition)** (एक विशिष्ट कारण) जो अपनी श्रेणी को लपेटती है, ताकि एक मिलान जिस भी स्तर पर आपको ज़रूरत हो वहाँ काम करे:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string; func backOffAndRetry(); func fixToolWiring(); func alertOps() -->
```go
_, err := a.Run(ctx, runID, input)
switch {
case errors.Is(err, agent.ErrModel):       // any provider fault (HTTP status, decode, stream)
	backOffAndRetry()
case errors.Is(err, agent.ErrUnknownTool):  // a specific condition (implies agent.ErrTool)
	fixToolWiring()
case errors.Is(err, agent.ErrStorage):      // durable-store I/O
	alertOps()
}
```

श्रेणियाँ: `ErrConfig`, `ErrModel`, `ErrTool`, `ErrStorage`, `ErrProtocol`, `ErrBudget`। स्थितियाँ (हर एक एक श्रेणी को लपेटती है): `ErrUnknownTool`, `ErrToolArgs` (`ErrTool` को लपेटती हैं), `ErrToolReinvoked`, `ErrInvalidApproval`, `ErrAlreadyDecided` (`ErrConfig` को लपेटती हैं), `ErrNoRecordedOutput`, `ErrIncompleteResponse` (`ErrModel` को लपेटती हैं), `ErrTruncatedToolArgs` (`ErrProtocol` को लपेटती है), `ErrBudgetExceeded`, `ErrMaxTurns` (`ErrBudget` को लपेटती हैं)। प्रदाता अडैप्टर `*RateLimited` (HTTP 429, एक `RetryAfter` संकेत के साथ) और `*APIError` (अन्य non-2xx, `StatusCode` के साथ) भी लौटाते हैं, दोनों `ErrModel` को लपेटते हैं। टूलकिट जो भी त्रुटि लौटाता है (मॉडल, MCP, स्टोर, और शासन अडैप्टरों से सहित) एक श्रेणी वहन करती है, इसलिए `errors.Is` पूरी सतह पर विश्वसनीय है।

और **नियंत्रण-प्रवाह संकेत** एक श्रेणी से समृद्धतर हैं, इसलिए वे ठोस प्रकार बने रहते हैं जिन्हें `errors.As` मिलाता है: `*ApprovalPending` (अनुमोदन आवश्यक), `*InterruptPending` (इंसानी इनपुट की प्रतीक्षा), `*TimerPending` (टिकाऊ टाइमर लंबित), `*SignalPending` (एक बाहरी सिग्नल की प्रतीक्षा), `*OutcomeUnknown` (पुनरारंभ असुरक्षित), `*SagaAborted` (वापस लुढ़काया गया), और `*HaltTooYoung` (`ResolveHaltRef` से, जब `WithMinHaltAge` अभी बीता नहीं है)। ये सभी सील किए गए इंटरफ़ेस `agent.Pause` को संतुष्ट करते हैं; `agent.IsPause(err)` से जाँचें और `agent.AsPause(err)` से पढ़ें। एक रुका या ठहरा हुआ रन एक "विफलता" श्रेणी नहीं है; `RunID` / `ToolUseID` / क्षतिपूर्ति विवरण के लिए struct का निरीक्षण करें। रद्दीकरण सामान्य `context.Canceled` / `context.DeadlineExceeded` के रूप में उभरता है, और जो ड्राइव अपने रन की लीज़ (`agent.Lease`) खो जाने के कारण रद्द हुई, वह `ErrLeaseLost` के रूप में; रद्दीकरण की तरह, यह कोई श्रेणी वहन नहीं करती।

## Middleware और अवलोकनीयता

दो स्वतंत्र `func(Handler) Handler` शृंखलाएँ उन दो सीमाओं पर जो मायने रखती हैं: मॉडल कॉल (`Use`) और हर टूल कॉल (`UseTool`)। पहले जोड़ा = सबसे बाहरी। दोनों *उत्परिवर्तनकारी और लघु-परिपथी* हैं: जो अंदर जाता है उसे दोबारा लिखें, जो बाहर आता है उसे रूपांतरित करें, या `next` को कॉल किए बिना लौटें।

<!-- docsnip: setup model agent.Model; store agent.Durable; tools []agent.Tool; import oteltrace "go.opentelemetry.io/otel/trace"; tracer oteltrace.Tracer -->
```go
var cost middleware.CostMeter
a := agent.New(model, store, tools...).
	WithTokenBudget(100_000). // per run, rebuilt from the journal on resume
	Use(
		middleware.Retry(3, middleware.WithBackoff(200*time.Millisecond, 10*time.Second)),
		middleware.Cost(&cost, middleware.Rates{InputPer1M: 3, OutputPer1M: 15}),
	).
	UseTool(middleware.ToolLog(log.Printf), middleware.ToolCache(), middleware.ToolRetry(3))

// opt-in OTel gen_ai.* spans (provider and model from agent.ModelInfoOf); the core has no OTel dependency:
a.Use(trace.Model(tracer))
a.UseTool(trace.Tool(tracer)) // execute_tool span per call; nests across the sub-agent boundary
// ... after the run: cost.Snapshot() (answer and spend, in tokens and USD)
```

`Retry` जिटर के साथ घातांकीय बैकऑफ़ करता है और एक प्रदाता 429 पर एक `Retry-After` का सम्मान करता है (अडैप्टर एक टाइप-किया गया `*agent.RateLimited` लौटाता है); `Cost` टोकन उपयोग (कैश-रीड/राइट सहित) से USD को एक `CostMeter` में जमा करता है जिसे आप रन के बाद पढ़ते हैं।

चूँकि `trace.Tool` लूप के भीतर चलता है, उसका span टूल को सौंपे गए संदर्भ में बैठता है, इसलिए जब एक टूल स्वयं एक उप-एजेंट होता है, तो उप-एजेंट का रन और उसके अपने span बच्चों के रूप में नेस्ट होते हैं। ट्रेस उप-एजेंट सीमा को स्वचालित रूप से पार करता है (ADK / AgenticGoKit / trpc-agent-go में एक कमी)।

टूल middleware टिकाऊ चरण के *भीतर* चलती है, इसलिए एक लघु-परिपथ (एक `ToolCache` हिट) या एक नीति अस्वीकृति किसी भी टूल परिणाम की तरह जर्नल होती है; पुनरारंभ इसे फिर से चलाता है और कभी middleware या टूल को दोबारा नहीं चलाता। `ToolRetry` और `ToolCache` केवल उन टूलों पर काम करते हैं जिनकी `Safety` इसकी अनुमति देती है (क्रमशः पुनः-प्रयास-सुरक्षित, और `ReadOnly`), और middleware चाहे जो करे, एजेंट एक ऐसे टूल को जो पुनः-प्रयास-सुरक्षित नहीं है, प्रति कॉल ज़्यादा-से-ज़्यादा एक बार चलाता है। `agent.ToolMiddleware` हस्ताक्षर के साथ अपनी लिखें:

<!-- docsnip: setup func authorized(context.Context, string) bool -->
```go
// Deny a tool by policy: the tool never executes; the model sees the error and reacts.
func RequireTag(tag string) agent.ToolMiddleware {
	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, tu agent.ToolUse) (json.RawMessage, error) {
			if !authorized(ctx, tag) {
				return nil, fmt.Errorf("tool %q denied: %w", tu.Name, agent.ErrTool)
			}
			return next(ctx, tu) // mutate tu.Args before, transform the result after
		}
	}
}
```

## मॉड्यूल

Bide एक बहु-मॉड्यूल रेपो है: एक निर्भरता-हल्का **कोर** (`github.com/bide-ai/bide`, यानी लूप, schema, middleware, मॉडल अडैप्टर, `plan` फ़्लो बिल्डर, `audit`; निर्भरताएँ केवल `x/sync` + `x/text` हैं) साथ ही प्रति भारी अडैप्टर एक मॉड्यूल (`mcp`, `trace`, `store/sqlite`, `store/postgres`, `govern/redislog`, `govern/sqlitelog`, `govern/postgreslog`, `codec/gcf`), और `govern` मॉड्यूल, जो gsm को वहन करता है और gsm के स्थिर होने तक v0.x पर रहता है। एक अडैप्टर import करें और आप उसका निर्भरता वृक्ष खींच लेते हैं; केवल कोर import करें और आप नहीं खींचते। एक केवल-कोर उपभोक्ता की बाह्य-मॉड्यूल सतह 2 है, 54 नहीं। देखें [docs/reference/module-structure.md](../../docs/reference/module-structure.md)।

## आर्किटेक्चर

निर्माण से षट्कोणीय (hexagonal): कोर पोर्ट परिभाषित करता है (`Model`, `Store`, `Tool`, `Middleware`); अडैप्टर किनारों पर प्लग होते हैं। निर्भरताएँ अंदर की ओर इंगित करती हैं; कोर किसी अडैप्टर और किसी अवसंरचना को import नहीं करता, जिसकी रक्षा `architecture_test.go` करता है।

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

## संघीय शासन: एजेंट जो सहमत होते हैं, प्रमाणनीय रूप से (Tier-2)

टिकाऊ कोर *एक* एजेंट के काम को क्रैश-सुरक्षित रखता है। `govern` स्तर दूसरे कठिन मामले को संभालता है: **अनेक स्वतंत्र रूप से चलाए गए एजेंट जिन्हें सहमत होना ही है**, प्रोसेस, टीम, या संगठनात्मक सीमाओं के आर-पार, बिना किसी केंद्रीय समन्वयक और बिना किसी एकल लेखक के। यह सहमति के दो सत्यापनीय रूप देता है, और दोनों में मुद्दा *सत्यापित करो, भरोसा मत करो* है: एक पक्ष सार्वजनिक कलाकृतियों से परिणाम जाँचता है, किसी और के एजेंट पर भरोसा किए बिना।

**साझा अवस्था पर सहमति (अभिसरण)।** साझा अवस्था को एक रजिस्ट्री के रूप में वर्णित करें (चर + अपरिवर्तनीय + घटनाएँ); gsm *निर्माण समय पर* सिद्ध करता है कि एजेंट क्रियाओं का हर अंतर्वयन उसी वैध अवस्था पर पहुँचता है, या निर्माण से इनकार करके आपको एक प्रति-उदाहरण दिखाता है। रनटाइम O(1) तालिका लुकअप है; अवस्था घटना-स्रोतित और क्रैश-पुनर्प्राप्य है। यह एक एकल साझा रजिस्ट्री से ऊपर की ओर **संघों (federations)** के आर-पार बढ़ता है (सीमा-पार बाधाएँ: वृक्ष, समाधानकर्ताओं के साथ बहु-स्रोत DAG, एकदिष्ट चक्रीय *जाल (meshes)*), `Embed` के माध्यम से संघटित होता है, और आपके लिए क्षतिपूर्ति को **संश्लेषित** भी कर सकता है (नियम घोषित करें, एक अभिसारी शासक पाएँ, या एक प्रमाण कि कोई मौजूद नहीं)। एजेंट `FederatedEventTool` के माध्यम से प्लग होते हैं, ताकि एक LLM टूल कॉल एक शासित घटना बन जाए।

**एक निर्णय पर सहमति (कोरम)।** k-of-n नामित मतदाता (हर एक एक मॉडल, प्रदाता, या प्रधान) एक सामान्यीकृत निर्णय डालते हैं; हर मत एक जर्नल-किया गया, ज़्यादा-से-ज़्यादा-एक-बार वाला चरण है जो रिकॉर्ड करता है कि किसने कैसे मत दिया, और k-of-n द्वार मत-गणना पर एक gsm अपरिवर्तनीय है, ताकि "k सहमत हुए" हर संभव गणना पर मशीन-जाँचा जाए। `bide-audit verify-quorum` सार्वजनिक कलाकृतियों से गणना और हर मत को पुनः-जाँचता है, उत्पादक पर भरोसा किए बिना बहुलता नियम को पुनरुत्पादित करता है। दावा सटीक है: एक कोरम सिद्ध करता है *कि k मतदाता सहमत हुए* और एकल-मॉडल जोखिम कम करता है; यह प्रमाणित नहीं करता कि निर्णय सही है (सहसंबंधित त्रुटियाँ स्वतंत्रता नहीं हैं), और केवल सामान्यीकृत निर्णयों को ही कोरम-किया जा सकता है, मुक्त-रूप गद्य को नहीं।

<!-- docsnip: setup ctx context.Context; machine *gsm.Machine; import "github.com/blackwell-systems/gsm"; log govern.EventLog -->
```go
gov, _ := govern.NewPersistent(ctx, machine, log, "order-42", machine.NewState())
tool := govern.EventTool(gov, "pay", "mark the order paid", "pay", agent.Safety{})
// hand `tool` to the agent: concurrent agents sharing `gov` converge, durably.
```

> पूरी गाइड, क्षमता सीढ़ी, और चलाने योग्य डेमो (`examples/govern/mesh`, `examples/govern/compose`, `examples/govern/quorum`) **[docs/guides/governance.md](../../docs/guides/governance.md)** में।

## गाइड

यहाँ नए हैं? **[शुरुआत करना](../../docs/getting-started.md)** से शुरू करें, पूरे नक़्शे के लिए **[docs इंडेक्स](../../docs/README.md)** इस्तेमाल करें, और शब्दावली (journal, at-most-once, lease, Waker, gsm, ProofBundle) के लिए **[अवधारणाएँ](../../docs/CONCEPTS.md)** देखें। सटीक टिकाऊपन गारंटी **[गारंटी](../../docs/GUARANTEE.md)** में कथित है और उसकी सीमाएँ **[ज्ञात सीमाएँ](../../docs/KNOWN-LIMITATIONS.md)** में।

**लेखन**

- **[फ़्लो](../../docs/guides/flows.md)**: `plan` टाइप-किया गया फ़्लो बिल्डर। टोपोलॉजी (`Step`/`Tool`/`Model`/`Switch`/`Join`/`LoopBack`) लिखें जो उसी जर्नल पर उतरती है, फिर सिद्ध करें कि एक रन ने उसका पालन किया (`Conform`)। चलाने योग्य: `examples/plan`।
- **[टिकाऊ चरण](../../docs/guides/durable-steps.md)**: अपना खुद का टिकाऊ काम संघटित करें: `Step`, `Parallel`/`Task` फ़ैन-इन, सागा (`RunSaga`), और टिकाऊ टाइमर (`Sleep`/`WaitUntil`)। चलाने योग्य: `examples/parallel`।
- **[विश्वसनीयता](../../docs/guides/reliability.md)**: प्रति-प्रयास टाइमआउट, वर्गीकृत पुनः-प्रयास, हेज्ड मॉडल कॉल, रेट लिमिटिंग, और लागत ट्रैकिंग, और वे कैसे संघटित होते हैं। चलाने योग्य: `examples/hedge`।
- **[सिग्नल और एंबिएंट](../../docs/guides/signals.md)**: बाहरी घटनाएँ एक रन में: टिकाऊ टाइमर और `Waker`, human-in-the-loop (`Interrupt`/`AnswerInterrupt`), और टिकाऊ सिग्नल (अंदर कम-से-कम-एक-बार, लागू ठीक-एक-बार)। चलाने योग्य: `examples/signals`, `examples/interrupt`।
- **[मॉडल](../../docs/guides/models.md)**: Anthropic, OpenAI-संगत, और Gemini अडैप्टर: `WithBaseURL`, सैंपलिंग, प्रॉम्प्ट कैशिंग, टाइप-की गई त्रुटियाँ, और बहुविध छवि इनपुट।
- **[MCP](../../docs/guides/mcp.md)**: एक MCP सर्वर को रनटाइम टूल स्रोत के रूप में जोड़ें, साइड-इफ़ेक्ट-सुरक्षित पुनरारंभ के साथ; एक विश्वसनीय सर्वर के टूल annotation टूलों को दोबारा चलाने के लिए सुरक्षित चिह्नित कर सकते हैं।
- **[अवलोकनीयता](../../docs/guides/observability.md)**: एक लाइन में OTel gen_ai span (`trace.Instrument`): span वर्गिकी, उप-एजेंट नेस्टिंग, टोकन-से-लागत, और सामग्री-कैप्चर गोपनीयता डिफ़ॉल्ट। चलाने योग्य: `examples/observability`।
- **[मैसेजिंग](../../docs/guides/messaging.md)**: एक इनबाउंड webhook (Slack, Telegram, SMS, Discord) से एक एजेंट चलाएँ, पुनर्वितरण-सुरक्षित: एक पुनः-प्रयास किया गया webhook दो बार फ़ायर करने के बजाय पुनर्खेल करता है। चलाने योग्य: `examples/webhook`।
- **[डिबगिंग और पुनर्प्राप्ति](../../docs/guides/debugging.md)**: नियतात्मक पुनर्खेल (`Replay`), घटना पुनर्निर्माण (`ReplayEvents`), Mermaid रन आरेख, और क्रैश पुनर्प्राप्ति (`Recover`) जो अंतरायित रनों को फिर से चलाती है।

**जवाबदेही और शासन**

- **[ऑडिट](../../docs/guides/audit.md)**: प्रमाण-वाहक रन। एक रन एक पोर्टेबल `RunCertificate` भेजता है, जिसे `bide-audit verify-run` से ऑफ़लाइन जाँचा जा सकता है। चलाने योग्य: `examples/govern/proof-carrying-run`।
- **[प्रत्यायोजन](../../docs/guides/delegation.md)**: हस्ताक्षरित क्षमता अनुदान जिन्हें एक उप-एजेंट केवल संकीर्ण कर सकता है (`Grant`/`SignGrant`), ऑफ़लाइन सत्यापित (`VerifyDelegationChain`), साथ ही एक साफ़ ट्रेल से अर्जित प्राधिकार। चलाने योग्य: `examples/govern/delegation`, `examples/govern/authority`।
- **[सुरक्षा मॉडल](../../docs/guides/security-model.md)**: क्रिप्टोग्राफ़िक गारंटियों का सटीक दायरा (अखंडता, प्रामाणिकता, छेड़छाड़-स्पष्टता, अ-प्रत्याख्यान, चयनात्मक प्रकटीकरण) और क्या दायरे से बाहर है (गोपनीयता)। ट्रेल पर निर्भर होने से पहले पढ़ें।
- **[शासन](../../docs/guides/governance.md)**: Tier-2 शासित-अवस्था आधार (gsm)। साझा अवस्था को एक रजिस्ट्री के रूप में वर्णित करें, और `Build()` सिद्ध करता है कि हर अंतर्वयन अभिसरित होता है या एक प्रति-उदाहरण लौटाता है। चलाने योग्य: `examples/govern/mesh`, `examples/govern/compose`।
- **[अनुमोदन](../../docs/guides/hitl-approval.md)**: एक टूल के चलने से पहले टिकाऊ इंसानी हस्ताक्षर-स्वीकृति, 1-of-1 से हस्ताक्षरित m-of-n तक (`ApprovalPolicy`, `SubmitDecision`), इस ऑफ़लाइन प्रमाण के साथ कि k नामित अनुमोदकों ने क्रिया से पहले अनुमोदन दिया (`audit.ApprovalEvidence`, `audit.VerifyApprovals`)। चलाने योग्य: `examples/approval`।
- **[कोरम](../../docs/guides/quorum.md)**: शासित k-of-n मॉडल सहमति (`govern.Quorum`), गणना जर्नल में एंकर और ऑफ़लाइन पुनः-जाँचने योग्य (`bide-audit verify-quorum`)। चलाने योग्य: `examples/govern/quorum`।

**संदर्भ और आंतरिक**

- **[विस्तार बिंदु](../../docs/reference/extension-points.md)**: पोर्ट और अडैप्टर (`Model`, `Store`, `Tool`, `Compensator`, `Retriever`, `Anchor`, `EventStore`), एक "अपना खुद का स्टोर लागू करें" वॉकथ्रू के साथ।
- **[bide कैसे सत्यापित होता है](../../docs/testing/verification.md)**: विफल होते परीक्षण के बिना कोई फ़िक्स नहीं, म्यूटेशन जाँच, क्रैश और रद्दीकरण स्वीप, बलपूर्वक अंतर्वयन, अनुरूपता सूट, और CI क्या प्रवर्तित करता है।
- **[परीक्षण और साक्ष्य](../../docs/testing/testing.md)**: क्या परीक्षित है और कैसे, chaos क्रैश-इंजेक्शन बेंचमार्क, अवकल दैवज्ञ (differential oracle), RFC 6962 अनुरूपता, और `eval` पैकेज में प्रमाणनीय-बनाम-सांख्यिकीय सीमा।
- **[जर्नल संघनन](../../docs/design/compaction.md)** (डिज़ाइन नोट): एक असीमित जर्नल को ऑडिट रीढ़ के समावेशन और संगति प्रमाणों को तोड़े बिना संघनित करना।

## संपर्क

प्रश्न, प्रतिक्रिया, या bide इस्तेमाल करने में रुचि: **dayna@blackwell-systems.com**। सुरक्षा मुद्दे [SECURITY.md](../../SECURITY.md) (निजी रिपोर्टिंग) के माध्यम से भेजें।

[English](../../README.md) · [简体中文](README.zh-CN.md) · [Русский](README.ru.md) · [हिन्दी](README.hi.md) · **العربية**

<p align="center">
  <img src="../../assets/bide-banner.png" alt="Bide">
</p>

**بناء وكلاء ذكاء اصطناعي مُعمَّرين بلغة Go. آثار جانبية تُنفَّذ مرة واحدة على الأكثر.**

*توقّفٌ يمكنك رفعه خيرٌ من تنفيذٍ مزدوج لا يمكنك التراجع عنه.*

سجلٌّ واحد لا يقبل إلا الإلحاق (append-only journal)، وأربع ضمانات لا يجمعها أي إطار عمل آخر للوكلاء في مكتبة واحدة:
آثار جانبية تُنفَّذ **مرة واحدة على الأكثر**؛ وآلاف عمليات التشغيل المُعمَّرة المتزامنة **في عملية واحدة، بلا
عنقود (cluster)**؛ و**سِجِل تدقيق قابل للتحقق تشفيريًّا** (براهين Merkle وفق RFC 6962، يمكن فحصها دون
الوثوق بالمورّد)؛ وحالة مشتركة **مُثبَتة التقارب (provably convergent)**. تحصل على الأربعة جميعًا من آلية
واحدة، لا من أربعة أنظمة مُدمجة، بوصفها مكتبة Go خالصة. مبنيّ للوكلاء الذين يُحرّكون الأموال أو يمسّون السجلات
أو يعملون تحت التدقيق.

**مبنيّ للوكلاء المُحيطيّين (ambient agents).** الوكيل المُحيطي يعمل دون إشراف: ينام حتى يوقظه مُحفِّز (جدول
زمني أو حدث)، ويعمل على مدى ساعات أو أيام، ويتوقف ليسأل إنسانًا فقط حين يحتاج إلى حُكم، دون أن يراقب أحد كل
خطوة. وذلك هو بالضبط حين تتوقف خاصيات «مرة واحدة على الأكثر» والاستئناف عالي الإتاحة والأثر القابل للتحقق عن
كونها كماليات؛ فالوكيل الخلفي الذي يتصرّف دون مراقبة لا بد أن يكون آمنًا عند الانهيار، وآمنًا عند إعادة التحفيز،
وقابلًا للإثبات بعد الوقوع. يوفّر Bide دورة الحياة المُعمَّرة لهذا: مؤقّتات `Sleep`/`WaitUntil` مُعمَّرة، و`Waker`
قابل للتوصيل للإيقاظ الزمني أو المدفوع بالأحداث، و`Interrupt`/`Resume` مُعمَّر لإدخال العنصر البشري المُصنَّف
(typed human-in-the-loop)، وكلها على السجل نفسه. أنت تُحضِر مصدر التحفيز وواجهة الإشراف؛ ويحافظ زمن التشغيل
على صحة كل تشغيلة عبر النوم والانهيارات وتسليم العُقَد.

الحالة: **v0 عاملة**، مُتحقَّق منها من طرف إلى طرف حيًّا. تتطلّب **Go 1.27**.

## سجلّ واحد، أربع ضمانات

الجميع يشحن حلقة وكيل؛ وحلقتنا نحو ~40 سطرًا. المهم هو الركيزة تحتها: سجلّ مُعمَّر لا يقبل إلا الإلحاق تُشتَقّ
منه الضمانات الأربع جميعًا، فتحصل عليها من آلية واحدة بدل دمج أربعة أنظمة.

### 1 · مرة واحدة على الأكثر، لا مرة واحدة على الأقل (مقيسة، لا مُدَّعاة)

تستأنف Temporal وDBOS وtrpc-agent-go وADK وeino جميعها عبر **إعادة التشغيل**: يجب أن تكون الأنشطة/الخطوات
عديمة الأثر عند التكرار (idempotent)، فالأثر الجانبي غير المتكرّر بأمان (شحنة مالية، بريد، شحنة بضاعة) قد
يُنفَّذ مرتين عبر انهيار. بنينا اختبار أداء **عادلًا** لحقن الانهيارات ([`chaos/`](../../chaos)، النتائج عبر
مجموعات SDK في [`benchmarks/`](../../benchmarks/README.md)) يقود عملية `charge` غير متكرّرة بأمان عبر كل نقطة
انهيار. والرقم **هو** المنتج:

```
Bide      maxFired=1    ✓ at-most-once held
trpc-agent-go  maxFired=6    ✗ double-charged
adk-go         maxFired=4    ✗
langchaingo    maxFired=64   ✗
eino           maxFired=64   ✗
```

`maxFired` هو أكبر عدد مرات نُفِّذ فيها أثر جانبي واحد فعلًا. **1 صحيح؛ وأي رقم أعلى هو شحنٌ مزدوج.**
مُحوّلات المنافسين مُتحقَّق من أنها ليست خصومًا وهميين (لكلٍّ اختبار عدالة يُثبت أن استئنافه يعمل حقًّا).
القطعة التي لا يملكها أيٌّ منهم: **علامة محاولة (attempt marker)** مُعمَّرة تُكتَب قبل كتابة غير متكرّرة بأمان،
و**التوقّف عند نتيجة مجهولة (halt-on-unknown-outcome)** عند الاستئناف: إذا لم تُسجَّل نتيجة كتابة قط، يتوقف
التشغيل لقرار بشري بدل التخمين.

### حين تكون النتيجة مجهولة، يتوقف

الحالة الصعبة في «مرة واحدة على الأكثر» ليست الانهيار الذي تراه، بل الذي لا تراه: أثر جانبي غادر نداؤه
العملية لكن نتيجته لم تصل إلى السجل قط. تتيح علامة المحاولة لتشغيلة مُستأنَفة أن تميّز «لم يبدأ قط» من «بدأ،
والنتيجة مجهولة»، وتحسم الحالة المجهولة بتسلسل هرمي ثابت، لا بالتخمين أبدًا:

<p align="center">
  <img src="../../assets/resolution-ladder.png" width="820" alt="سُلَّم حسم النتيجة المجهولة: الأثر الآمن لإعادة المحاولة يُعاد تلقائيًّا ويُزيل المورّد التكرار؛ والأثر الذي ترك سجلًّا قابلًا للاستعلام يحسمه مُسوِّي (reconciler) تلقائيًّا؛ والنتيجة المجهولة حقًّا تتوقف وتنتظر إنسانًا. عند الالتباس التام، يتوقف.">
</p>

الطبقة التي تقع فيها أداةٌ يحدّدها `Safety` المُعلَن لها: وسمها للقراءة فقط، أو عديمة الأثر عند التكرار، أو
إعطاؤها مفتاح عدم تكرار (idempotency key)، يجعل النتيجة المجهولة تُعاد تلقائيًّا؛ وإن لم تُعلِن شيئًا من ذلك
فإنها تتوقف. أمان إعادة المحاولة اختياري بالتفعيل؛ والتوقّف هو الافتراض ما لم تُفعِّله، فمكتبةٌ غايتها كلها
«لا تنفيذ مزدوج أبدًا» تفترض الأمان لا التخمين.

معظم النتائج المجهولة لا تبلغ إنسانًا أبدًا: مفتاح عدم التكرار يتيح للمورّد إزالة تكرار إعادة محاولة آمنة،
وللأنظمة التي لا تملكه (البريد، الخدمات الداخلية) يحسم مُسوٍّ الخطوةَ من السجل الذي تركته
(`agent.ResolveHalt`). الإنسان هو الحدّ الأدنى، لا الافتراض.

> [!IMPORTANT]
> **القاعدة تحت ذلك:** حين يُحرّك فعلٌ أموالًا، أو يمسّ سجلًّا، أو يقع تحت التدقيق، وتكون النتيجة مجهولة
> حقًّا، فإن التوقّف هو النتيجة الصحيحة. توقّفٌ يستطيع إنسان أو مُسوٍّ رفعه خيرٌ من شحن مزدوج لا يستطيع أحد
> استرداده.

### 2 · التنفيذ المُعمَّر كمكتبة، لا كعنقود

تملك Temporal الضمانات لكنها تحتاج خادمًا + أسطول عمّال (workers) لتشغيلها. هنا تأتي من **مُحوّل مخزن تُشغّله
بالفعل** (SQLite محليًّا، Postgres في الإنتاج). برنامج «hello-world» يستورد **المكتبة القياسية فقط**: لا
Temporal، ولا gRPC، ولا قاعدة بيانات متجهية مسحوبة إلى ثنائيّتك (مفروض عبر `architecture_test.go`).
استورده؛ لا تُشغّله.

ولأنه مكتبة Go، فإن عملية واحدة تُبقي عددًا كبيرًا جدًّا من هذه التشغيلات المُعمَّرة في الطيران دفعة واحدة.
عمل الوكيل مقيَّد بالإدخال/الإخراج (انتظار نداءات النموذج والأدوات)، وهو ما تمتصّه الـ goroutines دون عنقود.
تقيسه أداة [`cmd/bench`](../../cmd/bench/README.md): 20,000 تشغيلة، 5,000 منها في الطيران في آنٍ واحد، تحجب
كلٌّ منها ~100 مللي ثانية على النموذج، تنتهي في **نحو نصف ثانية (~470 مللي ثانية) من الزمن الجداري على جهاز Mac بمعالج Apple silicon من 10 أنوية، ونحو ثانية واحدة على مُشغِّل CI قياسي بأربع وحدات vCPU** على بضعة
آلاف من الـ goroutines وعشرات الميغابايتات (`go run ./cmd/bench -runs 20000
-concurrency 5000 -latency 50ms`). المكسب
هو الإنتاجية والبساطة التشغيلية، لا زمن استجابة أقل من النموذج (المورّد يملك زمن استجابة كل نداء)؛ وعند
التوسّع العالي يكون سقفُ إنتاجيةِ كتابةِ المخزن المُعمَّر هو الحد، لا الـ goroutines. كل تشغيلة متزامنة تُبقي
الضمانات الأربع. والموثوقية تحت ذلك الحِمل مبنيّة داخليًّا: **مهلات (timeouts)** لكل محاولة، وإعادة محاولة مع
تراجع (backoff) **تُصنِّف** الأخطاء العابرة مقابل النهائية، ونداءات نموذج **مُتحوَّطة (hedged)** (سباق مع نسخة
احتياطية، خُذ الأولى، لأجل زمن الذيل والاحتياط عن المورّد)، و**مُحدِّد معدّل (rate limiter)** لنداءات النموذج
والأدوات ([middleware](../../middleware)، [docs/guides/reliability.md](../../docs/guides/reliability.md)).

لأجل الإتاحة العالية، تستأنف أي عُقدة أي تشغيلة من المخزن المشترك، وتنسّق المشغّلات المتنافسة عبر **حجز
(lease)** لكل تشغيلة (`agent.Lease`): في العادة تقود عملية واحدة تشغيلة في المرة الواحدة، وينتهي حجز
الحائز المنهار فيتولّاها `agent.RecoverLoop` في عُقدة أخرى. قد يستيقظ حائز توقّف بعد انتهاء حجزه وهو ما زال
يقود التشغيلة، لكنه لا يستطيع إطلاق أثر جانبي مرة ثانية: «مرة واحدة على الأكثر» تقوم على مطالبة المحاولة
(attempt claim)، لا على الحجز. كالضمان 1، هذا مُتحقَّق لا مُدَّعى:
الاستبعاد المتبادل للعمّال المتزامنين، والانهيار-والتولّي، و«مرة واحدة على الأكثر» تحت مشغّلات متزامنة على
المخزن في الذاكرة (`agent/ha_e2e_test.go`)، و«مرة واحدة على الأكثر» عبر العمليات على Postgres، بمثيلَي مخزن
يتشاركان قاعدة بيانات واحدة (`TestPostgres_HAAtMostOnceAcrossInstances` في `store/postgres/postgres_test.go`؛
خلفية Postgres تُنفِّذ الحجز عبر upsert بساعة قاعدة البيانات).

### 3 · عمود فقري للتدقيق قابل للتحقق تشفيريًّا، من السجل نفسه

<p align="center"><img src="../../assets/merkle.png" width="820" alt="برهان شمول Merkle: سجلّ في السجل (charge) تصعد تجزئته عبر مسار أشقّائه إلى الجذر الموقَّع، مُثبِتةً أن السجلّ ضمن التاريخ المُلتزَم به بينما تبقى السجلات الأخرى مخفيّة."></p>

السجل الذي يجعل الاستئناف آمنًا **هو** سجل التدقيق، وهو مُلتزَم بـ**التشفير نفسه الذي تستخدمه Certificate
Transparency** ([RFC 6962](https://datatracker.ietf.org/doc/html/rfc6962)، مفحوصًا مقابل متجهات المرجع
المنشورة). التمييز المهم لمشترٍ خاضع للتنظيم: هذا **قابل للتحقق، لا مُسجَّل فحسب**. يفحص طرفٌ ثالث برهانًا
*دون الوثوق بك أو بقاعدة بياناتك أو بسجلاتك*:

- **برهان الشمول (Inclusion proof)**: إثبات وقوع فعل محدّد بعينه (هذه الشحنة، هذه الموافقة) بتعقيد
  O(log n)، دون كشف أي شيء آخر. إفصاح انتقائي لمُدقِّق.
- **برهان الاتّساق (Consistency proof)**: إثبات أن التاريخ لم يُلحَق به إلا إلحاقًا، ولم يُعَد كتابته أو
  ترتيبه قط.
- **رأس شجرة موقَّع + تثبيت مستمر**: يُوقّع `AuditedStore` التزامًا لكل خطوة وينشره خارج النطاق إلى سجل
  شفافية خارجي؛ فيصبح العبث قابلًا للإثبات، لا مجرد مشكوك فيه.
- **مَن تصرّف، وبأي سُلطة**: يمكن للورقة نفسها أن تلتزم بالهوية الفاعلة (الفاعل، ونيابةً عمّن، وتحت أي
  تفويض موقَّع) وأن تفرض السلطة المُفوَّضة كحُكم مُحكَّم (governed invariant)، فيُظهِر البرهان ليس فقط ما حدث
  بل من كان مُخوَّلًا له. أحضِر مزوّد الهوية (IdP) الخاص بك؛ هذا يجعل الفعل المُخوَّل قابلًا للإثبات، وهو لا
  يحل محل المصادقة.

**براهين تتحقق منها، لا سجلات تثق بها.** يقدّم الجميع سواه *قابلية الملاحظة (observability)* (سجلات تثق بها
لأن المورّد حاصل على SOC2)؛ وهذا *برهان تشفيري تفحصه بنفسك*. أنتِج `ProofBundle` قابلًا للنقل لفعل واحد
(`audit.ProveToolCall`)، أو `EvidencePackage` لتشغيلة كاملة (`audit.Evidence`) يجمع برهان كل فعل جوهري في
ملف واحد، وسلّمه إلى مُدقِّق يتحقق منه دون اتصال بـ `bide-audit verify` / `verify-evidence` أو بمُتحقِّق
يعتمد المكتبة القياسية فقط ولا يستورد الـ SDK أبدًا. **لا يملك أي إطار عمل وكلاء آخر هذا إطلاقًا.** يحمل العمود
الفقري نفسه بقية طبقة المساءلة، وكلها قابلة للتحقق دون اتصال: تشغيلات حاملة للبرهان (شهادة `RunCertificate`
واحدة تُثبت التزام تشغيلة كاملة بالسياسة)، ومِنَح قدرات موقَّعة مع تفويض مُضيِّق، وسُلطة مكتسَبة من سجل تدقيق
نظيف، ونِصاب (quorum) محكوم من نوع k-of-n. ← [docs/guides/audit.md](../../docs/guides/audit.md)

### 4 · حالة مشتركة مُثبَتة التقارب (gsm)

طبقة الحالة المحكومة: عدة عمليات تُعيد تشغيل السجل المُعمَّر نفسه **تتقارب على حالة متطابقة**، مدعومةً
بـ**برهان مفحوص آليًّا**. نظام إعادة الكتابة التطبيعي (normalization rewrite system) في محرك التقارب **gsm**
مُلتقٍ (confluent)، فترتيب إعادة تشغيل الخطوات لا يمكن أن يُغيّر النتيجة. البرهان خالٍ من البدهيّات
(axiom-free) ومُتحقَّق في الـ CI على Coq 8.18 و8.20 (يُبلِّغ `Print Assumptions` بـ «Closed under the
global context»): [برهان Coq/Rocq](https://github.com/blackwell-systems/normalization-confluence/tree/main/coq)
([![verify](https://github.com/blackwell-systems/normalization-confluence/actions/workflows/verify.yml/badge.svg)](https://github.com/blackwell-systems/normalization-confluence/actions/workflows/verify.yml)).
والبرهان لا يجلس فقط بجوار الشفرة: يُعاد التصديق على حُكم gsm لكل آلة **عبر مُتحقِّقَين مستقلَّين مُستخرَجَين
من ذلك البرهان** (أحدهما يعيد حساب التقارب من جداول الخطوات المُصدَرة، والآخر مباشرة من القواعد)، فلا يمكن
لعلّة في مُتحقِّق gsm المكتوب بـ Go أن تُمرّر آلةً غير متقاربة. تُعبَّر القواعد كـ**بيانات تجميع قابلة للفحص
(inspectable combinator data)** بدل مُغلَقات مُبهَمة (opaque closures)، وهو ما يجعلها قابلة للتسلسل والنقل
وإعادة الفحص؛ ويمكن للتحقق أيضًا أن يجري **محليًّا على البصمة (footprint-local)** (`BuildCompositional`)
لتصديق آلات فضاء حالتها الكلي أكبر من أن يُعدَّد. هكذا تشترك وكلاء مستقلة في الحالة دون كاتب وحيد. والادّعاء
دقيق: *تقارب إعادة التشغيل مستقلًّا عن الترتيب*، مُبرهَن، لا «الوكلاء يتفقون دائمًا». والنتيجة الاتحادية
مُميكَنة بالكامل، بما فيها استقلالية الترتيب اللامتزامنة (الفوضوية).

مُجسَّد على نطاق واسع: اختبار تكامل يقود حتى **10,000,000 وكيل محكوم، 2,048 منها في آنٍ واحد،** عبر ترتيبات *عشوائية تنتهك
الأحكام*، (كل تشغيلة تخرق حُكمًا مُقيَّدًا فتُعوَّض)، ويؤكّد أن كل وكيل يتقارب على الصورة الطبيعية الصحيحة
نفسها *و*يُنتج برهان تدقيق يتحقق دون اتصال، في عملية واحدة بكومة حيّة مسطّحة نحو ~3 ميغابايت (~13 دقيقة،
~12.5 ألف وكيل/ث). هذا اختبار على مستوى الإطار (نموذج وهمي، مخزن في الذاكرة): يمرّن آليات الحوكمة والتدقيق على
نطاق واسع، لا نموذج LLM حيًّا أو قاعدة بيانات إنتاجية. انظر [docs/testing/testing.md](../../docs/testing/testing.md).

### مقابل أزمنة تشغيل التنفيذ المُعمَّر والوكلاء

| | **Bide** | Temporal / DBOS | ADK · eino · trpc · langchaingo |
|---|---|---|---|
| أثر جانبي غير متكرّر بأمان عند الانهيار | **مرة واحدة على الأكثر (يتوقف عند نتيجة مجهولة)** | مرة واحدة على الأقل؛ يجب أن تكون الأنشطة/الخطوات عديمة الأثر عند التكرار | مرة واحدة على الأقل؛ إعادة تشغيل (**مقيسة 4–64×**) |
| النشر | **مكتبة + قاعدة بيانات تُشغّلها بالفعل** | خادم + أسطول عمّال | مكتبة |
| تدقيق يُظهِر العبث | **عمود Merkle وفق RFC 6962 (السجل نفسه)** | غير مبنيّ داخليًّا | لا شيء |
| حالة مشتركة متقاربة | **مُثبَتة (gsm)** | غير متوفّر | لا شيء |

### الحِرفة تحت السطح

بعد الضمانات الأربع، التفاصيل التي تجعل البناء عليه ممتعًا:

- **Go خالصة افتراضيًّا، مع باني تدفّق مُصنَّف اختياري.** تكتب `if`/`for`/دوال ويكون الرسم البياني عرضًا
  *مُشتَقًّا* (`RenderMermaid`، `Topology`)، لا شيئًا تُجبَر على تأليفه. وحين تريد طوبولوجيا مؤلَّفة، يمنحك
  باني `plan` ذلك ويُنزِله إلى زمن التشغيل نفسه. انظر [الرسوم البيانية (Graphs)](#الرسوم-البيانية-graphs).
- **استدلال Claude ينجو من الرحلات ذهابًا وإيابًا.** تُحفَظ تواقيع التفكير المُوسَّع (extended-thinking)؛
  إذ تُسقطها معظم الـ SDKs، فتكسر بصمت مزيج التفكير + استخدام الأدوات.
- **مخططات أدوات واعية بالمورّد.** مخطط واحد مُنعكِس (reflected)، يُصدَر لكل لهجة (وضع OpenAI الصارم، إلخ)،
  لا مخطط عام واحد يرفضه الوضع الصارم وGemini.
- **أي نموذج، مُحوّل واحد.** Claude الأصلي، وGemini الأصلي، وأي نقطة نهاية متوافقة مع OpenAI (OpenAI،
  Ollama، DeepSeek، Groq، OpenRouter، vLLM، Azure، xAI…) عبر `WithBaseURL`.
- **احتياط متعدد العُقَد، مُنسَّق.** تستأنف أي عُقدة أي تشغيلة (Postgres، بلا قفل كاتب وحيد)؛ ويمنع حجزٌ
  لكل تشغيلة المُستعيدين المتنافسين والعمّال الأحياء من القيادة المزدوجة، وتُتولَّى تشغيلات الحائز المنهار
  عند انتهاء الحجز.

## الرسوم البيانية (Graphs)

معظم أطر الوكلاء تجعل الرسم البياني *الأساس*: الشيء الذي يجب أن تؤلّفه والشيء الذي يُنفَّذ، بعُقَد وحوافّ
وكائن حالة، وأحيانًا بانٍ بصري فوق ذلك. يعكس Bide هذا. أسطح التأليف نفسها متاحة، وصولًا إلى بانٍ بصري وبما
يشمله، لكن كطبقات تختارها فوق ركيزة سجلّ بـ Go خالصة، لا كأساس أبدًا. والسبب دقيق لا أيديولوجي.

الرسم البياني لا يضيف قدرة تعبيرية. أي شيء يحسبه رسمٌ بياني يحسبه التحكّم العادي في التدفّق: فالرسم البياني
الحسابي هو رسم بياني للتحكّم في التدفّق، والتتابع والاختيار والتكرار تكفي للتعبير عن أيٍّ منها. ليس هناك سلوك
وكيل تستطيع بناءه كرسم عُقَد-وحوافّ ولا تستطيع كتابته بـ `if` و`for` ودوال. ما يضيفه الرسم البياني ليس قدرةً
بل *تجسيدًا (reification)*: تمثيل من الطبقة الأولى للتدفّق يمكنك فحصه وتصويره والتحقق منه ساكنًا وتأليفه خارج
الشفرة. وهذا مفيد فعلًا، لكنه طبقة أدوات، لا أساس، وليس مطلوبًا لبناء الوكلاء.

فالركيزة هنا Go خالصة، والضمانات (المعمورية، مرة واحدة على الأكثر، الأثر القابل للتحقق) تأتي من السجل، لا من
رسم بياني. ويبقى الرسم البياني موجودًا كعرض *مُشتَقّ*: `RenderMermaid` يعيد بناءه ممّا جرى فعلًا.

إن أردت رسمًا بيانيًّا تؤلّفه، فتلك الطبقة موجودة بالفعل: حزمة **`plan`** بانٍ تدفّق مُقيَّد ومفحوص الأنواع
يُترجَم إلى زمن التشغيل هذا ويرث «مرة واحدة على الأكثر» وأثر التدقيق مجانًا. تصِل عُقَدًا مُصنَّفة (`Step`،
`Tool`، `Model`، `Switch`، `Join` للتجميع، `LoopBack` المحدود) في `Flow`، أو تؤلّف الطوبولوجيا نفسها
كتهيئة تصريحية (`plan.Load`) يمكن لطبقة أعلى كبانٍ بصري أن تُصدرها. تبقى طبقةً تختارها، لا الأساس: إطار
«الرسم-البياني-أولًا» لا يستطيع أن يقدّم العكس، لأن الرسم البياني عنده هو الأساس لا خيار.

ولأجل زمن تشغيل للمساءلة، الاتجاه يهمّ كذلك. الرسم البياني المؤلَّف مخطّطٌ تثق به؛ والرسم البياني المُشتَقّ
مُعاد بناؤه من السجل، فهو بالضبط ما جرى. طبقة `plan` تربط الاثنين: يكشف `Topology()` و`RenderMermaid()`
الشكلَ المُعلَن، ويتحقق `Conform()` تشفيريًّا من أن تشغيلة اتّبعت الطوبولوجيا التي أعلنتها، بموقف «تحقّق، لا
تثق» نفسه كبقية النظام. والقاعدة التي تمنع أي طبقة كهذه من تشعيب زمن التشغيل هي أن السطح الجديد يجوز أن يضيف
طريقة للتأليف، لا طريقة للتنفيذ أبدًا: كل طبقة تُنزَّل إلى زمن التشغيل الوحيد المدعوم بالسجل. انظر
[docs/guides/flows.md](../../docs/guides/flows.md).

### ثلاث طرق للتأليف، زمن تشغيل واحد

تدفّق فرز الطلبات نفسه، بثلاث طرق. Go الخالصة هي الافتراض: اكتب تحكّمًا عاديًّا في التدفّق، وسمِّ الخطوات
التي يجب أن يجعلها السجل آمنة عند الانهيار.

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

وحين تريد التدفّق نفسه أثرًا من الطبقة الأولى قابلًا للفحص، يصِل باني `plan` عُقَدًا مُصنَّفة في `Flow`
يُنزَّل إلى زمن التشغيل نفسه:

```go
b := plan.New[Order, Receipt]("order-triage")
classify := b.Step("classify", func(o Order) (Assessment, error) { ... })
reserve  := b.Step("reserve",  func(a Assessment) (Reservation, error) { ... }) // non-idempotent
finalize := b.Step("finalize", func(r Reservation) (Receipt, error) { ... })
decline  := b.Step("decline",  func(a Assessment) (Receipt, error) { ... })

b.Switch(classify,
    plan.When(func(a Assessment) bool { return a.Rush }, reserve),
    plan.Else(decline),
)
b.Edge(reserve, finalize)

flow, err := b.Build() // inherits at-most-once and the audit trail
```

أو ألِّف الطوبولوجيا نفسها كتهيئة تصريحية يمكن لطبقة أعلى (بانٍ بصري) أن تُصدرها، تُحمَّل بـ `plan.Load`:

```json
{
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

```go
flow, err := plan.Load[Order, Receipt](configBytes, reg) // same topology, same Digest()
```

الثلاث جميعًا تُنزَّل إلى زمن التشغيل الوحيد المدعوم بالسجل، فتحصل على «مرة واحدة على الأكثر» والاستئناف
عالي الإتاحة والأثر القابل للتحقق مجانًا أيًّا كان السطح الذي تختاره.

## التشغيلات المُحيطية: نوم واستيقاظ ومقاطعة مُعمَّرة

الضمانات الأربع أعلاه هي الركيزة؛ وهذه هي دورة الحياة التي تُمكّنها. التشغيلة المُحيطية لا تجلس في حلقة دردشة
متزامنة. تنام، وتستيقظ على مُحفِّز، وتتوقف لإنسان، وكلٌّ من تلك التحوّلات خطوةٌ مُعمَّرة «مرة واحدة على الأكثر»
على السجل، فتنجو التشغيلة من الانهيارات وتسليم العُقَد بينها.

- **نم حتى موعد نهائي.** يوقِف `Sleep`/`WaitUntil` تشغيلة ويُسجِّل وقت استيقاظها، فيبقى التوقّف بعد إعادة
  التشغيل. وإعادة الاستدعاء عند وقت الاستيقاظ تستأنف مرة واحدة بالضبط.
- **استيقظ بالزمن أو الحدث.** `Waker` قابل للتوصيل (`MemWaker` داخل العملية افتراضيًّا) يعيد استدعاء تشغيلة
  مُستحقّة؛ ومصدر التحفيز لك (حلقة داخل العملية، أو cron، أو طابور، أو webhook وارد)، فتقود الركيزة نفسها
  الوكلاء المُجدوَلين والمدفوعين بالأحداث معًا.
- **قاطِع لأجل إنسان، بمعمورية.** يوقِف `Interrupt[T]`/`Resume` تشغيلة عند أي نقطة لطلب قرار مُصنَّف
  ويستأنف بجواب الإنسان كخطوة مُسجَّلة (انظر [العنصر البشري في الحلقة](#العنصر-البشري-في-الحلقة-human-in-the-loop)).
  الموافقة/الرفض هي الحالة البوليانية الخاصة.

أنت تُزوّد مصدر التحفيز وسطح الإشراف؛ ويُبقي زمن التشغيل التشغيلة صحيحة عبر كل نوم واستيقاظ ومقاطعة وانهيار
وتسليم. قابل للتشغيل في `examples/signals` (توصيل حدث إلى تشغيلة منتظِرة)، و`examples/interrupt` (توقّف/استئناف
مع العنصر البشري)، و`examples/recover` (استئناف مُعمَّر). انظر [دليل الإشارات والوكلاء المُحيطيّين](../../docs/guides/signals.md).

## الضمان 1، بالشفرة: لن يشحن مرتين

```go
// A tool that moves money is a write: not ReadOnly, not Idempotent.
charge := agent.Func("charge_card", "Charge the customer", agent.Safety{},
	func(ctx context.Context, in ChargeArgs) (Receipt, error) { /* ... */ })

// If the process crashes after the charge fires but before its result is journaled,
// resume does NOT run it again: it returns *ResumeHalt so you confirm, not double-charge:
_, err := a.Run(ctx, runID, input)
var halt *agent.ResumeHalt
if errors.As(err, &halt) {
	// halt.ToolName == "charge_card": outcome unknown, a human decides, no double side effect.
}
```

## البداية السريعة

تتطلّب Go 1.27 (النواة تستخدم توابع مُعمَّمة). إن كان `go version` أقدم، فرقِّ أو اضبط
`GOTOOLCHAIN=go1.27.0`.

حزمة النواة هي `agent`، تُستورَد من `github.com/bide-ai/bide/agent`
(كما تُظهِر الكتلة أدناه).

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

شغّل مثال الاختبار الحيّ: `OPENROUTER_API_KEY=sk-... go run ./examples/smoke`

تُرجِع `Run` الرسالة النهائية فقط. للحصول على ملخّص تشغيلة (استهلاك الرموز، مجموعًا عبر الأدوار، شاملًا
التخزين المؤقت؛ وعدد أدوار النموذج؛ ومدّة الزمن الجداري) استخدم `RunResult` (و`RunSagaResult`):

```go
res, err := a.RunResult(ctx, runID, input)
// res.Message, res.Usage, res.Turns, res.Duration, res.RunID
```

## البثّ (Streaming)

تحجب `Run` وتُرجِع الجواب النهائي. لمراقبة الوكيل وهو يعمل (فروق الرموز، حدود الأدوار، بدء/انتهاء الأداة)،
استخدم `Stream`. تقود **الحلقة نفسها** (`Run` حرفيًّا هي `Stream(...).Final()`)، فالمعمورية والاستئناف
وأمان الأثر الجانبي متطابقة:

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
answer, err := stream.Final() // terminal message + error (incl. *PendingApproval / *ResumeHalt)
```

الأحداث: `TurnStarted`، `ModelEvent` (تغذية الرموز)، `AssistantTurn`، `ToolStarted` / `ToolCompleted`،
`ApprovalRequired`، `Finished`. مرِّر على `Events()` لأجل واجهة ثم استدعِ `Final()`، أو استدعِ `Final()`
وحدها لتتصرّف تمامًا كـ `Run` (تستنزف الأحداث نيابةً عنك).

شيئان جديران بالمعرفة، وكلاهما نتيجة للمعمورية:
- **فروق الرموز تصل أسفل سلسلة الـ middleware** (يظل Retry / Cost يريان الرسائل المُجمَّعة كاملة)،
  و**فقط عند نداء نموذج جديد**.
- **عند الاستئناف، يُعاد إصدار النصّ المُسجَّل** كـ `AssistantTurn{Replayed: true}` + `ToolCompleted`
  قبل التقدّم الحيّ، فتعيد واجهةٌ جديدة بناء القصة كاملة بعد انهيار، والدور المُعاد لا يُنتج فروق رموز (كان
  قد حُسم بالفعل).

`StreamSaga` هو نظير البثّ لـ `RunSaga`.

## الخرج المُصنَّف

تُرجِع `RunTyped[T]` قيمة `T` مُصنَّفة بدل رسالة حرّة الصياغة. تحقن أداة `final_answer` اصطناعية مخطط JSON
لها مُشتَقّ من `T` (عبر حزمة `schema`) وتوجّه النموذج ليستدعيها متى أنجز عمله، فيستطيع وكيلٌ مستخدِمٌ للأدوات
أن يؤدّي عملًا حقيقيًّا *ثم* يجيب مُصنَّفًا. مُحايد للمورّد (مبنيّ على نداء الأدوات الأصلي، لا وضع JSON عند
مورّد بعينه).

```go
type Weather struct {
	City  string `json:"city"`
	TempF int    `json:"temp_f"`
}

w, err := agent.RunTyped[Weather](ctx, a, runID, "weather in SF?")
// w.City == "SF", w.TempF == 68
```

إنها دالة حزمة، لا تابع (توابع Go لا تستطيع إضافة معاملات نوع). القيمة تُفكَّك من نداء الأداة *المُسجَّل*،
فهي **آمنة عند الاستئناف**: انهيار في منتصف التشغيلة يستعيد الجواب المُصنَّف من السجل عند الاستئناف. وأول
نداء `final_answer` تقبله الأداة يُنهي التشغيلة. وفقط إن لم يُجرِ النموذج نداءً كهذا قط (فردّ بنصّ JSON عادي
بدلًا منه) تُحلّل `RunTyped` ذلك النصّ. يُقصَد بـ `T` أن يكون بنية (struct).

على المورّدين المتوافقين مع OpenAI ذوي المخرجات المُبنيَنة الصارمة، تستخدم `RunTypedNative[T]` صيغة استجابة
مخطط JSON الأصلية للمورّد بدل الأداة (المخطط مفروض من جهة المورّد، بلا رحلة أداة ذهابًا وإيابًا)؛ وAnthropic
تتجاهلها، فاستخدم `RunTyped` هناك لخرج محايد للمورّد.

## المعاينة (Sampling)

ضوابط التوليد محايدة للمورّد وتُضبَط مرة واحدة؛ ويربطها كل مُحوّل على صيغة سلكه (ويُسقِط ما لا يستطيعه، مثل
عدم امتلاك Anthropic لـ `seed`):

```go
a := agent.New(model, store, tools...).
	WithSampling(agent.Temperature(0), agent.MaxTokens(500), agent.TopP(0.9), agent.Seed(42))
```

الحقول اختيارية بالتصميم: الحقل غير المضبوط يستخدم افتراض المورّد، فـ `Temperature(0)` الصريحة مميَّزة عن
«غير محدَّد». و`MaxTokens` على مستوى الطلب تتجاوز افتراض المُحوّل وقت البناء.

## التخزين المؤقت للموجّهات (Prompt caching)

تعيد حلقة الوكيل إرسال بادئة ثابتة كبيرة (موجّه النظام + مخططات الأدوات) كل دور. يُحاسِب التخزين المؤقت
لموجّهات Anthropic تلك التكرارات بمعدّل القراءة من التخزين المؤقت:

```go
model := anthropic.New(key, anthropic.WithPromptCache())
```

هذا يضع نقاط قطع `cache_control` على كتلة النظام وتعريفات الأدوات. وOpenAI تخزّن البادئات تلقائيًّا (بلا
حاجة إلى علَم). في كلتا الحالتين، تظهر فاعلية التخزين المؤقت في `agent.Usage` (`CacheReadTokens`، مُقدَّمة
من التخزين المؤقت، و`CacheWriteTokens`، مكتوبة إليه)، فيرى حساب التكلفة والتتبّع وميزانية رموز التشغيلة
الأرقام الحقيقية.

## الجلسات (متعدّدة الأدوار)

`Run` دورٌ واحد. `Session` محادثة مُعمَّرة متعدّدة الأدوار: كل `Send` تشغيلة وكيل كاملة (أدوات، استئناف،
أمان أثر جانبي) مُغذّاة بالنصّ حتى الآن، فيتذكّر الوكيل الأدوار السابقة.

```go
s, _ := a.Session(ctx, "user-42")   // reopens + rebuilds the transcript from the store
a1, _ := s.Send(ctx, "what's the capital of France?")
a2, _ := s.Send(ctx, "and its population?")   // sees turn 1 in context
```

يُسجَّل النصّ دورًا-بدور تحت مُعرّف الجلسة، فتعيد عملية أُعيد تشغيلها `a.Session(ctx, "user-42")` بناءه
وتُكمِل. يعمل الدور N تحت `"<id>/tN"` (بسجلّه المُعمَّر الخاص للاستئناف عبر الانهيار *داخل* دور)؛ والذاكرة
المحادثية هي نصّ السؤال/الجواب: تبقى نداءات الأداة الوسيطة لدور داخل ذلك الدور ولا تتسرّب إلى لاحقيه. وإن
توقّف دور (موافقة / `Interrupt`)، تُرجِع `Send` ذلك الخطأ؛ فحلّه واستدعِ `Send` مجدّدًا بالمدخل نفسه للاستئناف.
وحتى ذلك الحين، تُرجِع `Send` برسالة مختلفة `ErrConfig`: الدور المفتوح يخصّ رسالته. وللرسائل الواردة التي
قد يُعاد تسليمها، تُجيب `SendOnce(ctx, id, text)` عن كل مُعرّف رسالة مرة واحدة. وعدّة مقابض على جلسة واحدة لا
تُضيِّع دورًا أبدًا ولا تُجيب رسالةً بردّ رسالة أخرى.

## قابلية التدقيق (سجلّ يُظهِر العبث)

السجل المُعمَّر يسجّل بالفعل كل خطوة من تشغيلة. تلتزم حزمة `audit` بذلك التاريخ عبر سلسلة تجزئة (hash chain)،
فيصبح تنفيذ التشغيلة قابلًا للتحقق:

```go
head, _ := audit.Head(ctx, store, runID)     // SHA-256 chain over the journal (persisted order)
sig := audit.Sign(head, priv)                // anchor it: sign / publish out-of-band
```

أي تعديل / إدراج / حذف / إعادة ترتيب لسجلّ يُغيّر الرأس (head). **نموذج الأمان:** يمنح هذا سلامةً غير
مشروطة، وإظهارًا للعبث *حين تُثبّت الرأس خارج النطاق* (سلسلة في قاعدة البيانات نفسها التي يتحكّم بها مهاجم
يمكن إعادة كتابتها وإعادة تجزئتها)؛ انظر وثيقة الحزمة. إنه اللُّحمة (seam) بين الامتثال/المؤسسة: آثار جانبية
«مرة واحدة على الأكثر» قابلة للإثبات *مع* سجلّ قابل للتحقق لما فعله الوكيل بالضبط.

لأجل **الإفصاح الانتقائي**، يبني `audit.Root` / `Prove` / `VerifyInclusion` شجرة Merkle وفق **RFC 6962**
(Certificate Transparency)، فتستطيع إثبات أن سجلًّا واحدًا جزء من تشغيلة مُلتزَمة عبر برهان شمول بتعقيد
O(log n)، *دون كشف السجلات الأخرى* (مثلًا: أرِ مُدقِّقًا أن شحنة واحدة حدثت، دون كشف أي عملاء أو موجّهات
أخرى). ويُثبِت `ProveConsistency` / `VerifyConsistency` أن جذرًا أسبق هو **بادئة لا تقبل إلا الإلحاق** لجذر
لاحق: أن التاريخ أُلحِق فقط، لم يُعَد كتابته أو ترتيبه (ضمان سجل الشفافية). التنفيذ مفحوص مقابل متجهات اختبار
RFC 6962 المنشورة.

يُنتج `SignTreeHead` **رأس الشجرة الموقَّع** بأسلوب CT، `{Size, Root, Timestamp}` موقَّعًا بـ Ed25519، وهو
الأثر الذي تنشره. التدفّق الكامل: وقّع رأس شجرة (STH)، ثم أفصِح لاحقًا عن سجلّ واحد ببرهان شمول يفحصه مُدقِّق
مقابل الجذر الموقَّع، وأثبِت النموّ بالإلحاق فقط بين رأسَي شجرة (STHs). انظر
[docs/guides/audit.md](../../docs/guides/audit.md) للنموذج والواجهة البرمجية وتدفّق الامتثال من طرف إلى طرف.

## RAG والذاكرة (أحضِر خاصّتك)

لا يشحن Bide **مخزنًا متجهيًّا ولا مُضمِّنًا (embedder) ولا خلفية ذاكرة**: يمنحك *اللُّحمة* وتوصِل المخزن
الذي تُشغّله بالفعل. نفِّذ واجهة واحدة مقابل بنيتك التحتية:

```go
type Retriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]agent.Doc, error)
}
```

ثم وصِّلها بإحدى طريقتين:

```go
// Agentic RAG: the model searches on demand:
a := agent.New(model, store, agent.RetrievalTool(myStore, 5))

// Classic RAG: top-k auto-injected as context on each user turn:
a.Use(agent.WithRetrieval(myStore, 5))
```

الذاكرة المحادثية مبنيّة داخليًّا بالفعل (`Session`)؛ والسياق الديناميكي يمرّ عبر `WithSystemPromptFunc`؛
وهذه اللُّحمة تغطّي الذاكرة الدلالية / طويلة الأمد. ومُحوّلات المخازن الملموسة (إن لزمت يومًا) ستكون وحدات
منفصلة، لا في النواة أبدًا. انظر [docs/guides/rag-memory.md](../../docs/guides/rag-memory.md).

## أمان الاستئناف، في جدول واحد

```go
agent.Safety{ReadOnly: true}          // no side effects → always safe to re-run
agent.Safety{Idempotent: true}        // safe to retry (dedupes downstream)
agent.Safety{}                        // a write → HALT on unknown outcome, don't double-fire
agent.Safety{RequiresApproval: true}  // pause for human approval before executing
```

قبل أثر جانبي غير متكرّر بأمان، تُسجّل الحلقة *علامة محاولة* مُعمَّرة، فيميّز الاستئناف بين «لم يُنفَّذ قط»
(آمن للتنفيذ) و«نُفِّذ وانهار» (توقّف): بدقّة، لا تحفّظًا.

هذا **مُبرهَن، لا مُدَّعى.** `dst_test.go` اختبار محاكاة حتمية: مخزنٌ يحقن الأعطال ينهار عند *كل* نقطة كتابة
(وعبر مئات جداول الانهيار المتعدّد العشوائية)، ويؤكّد المُختبِر أن أثرًا جانبيًّا غير متكرّر بأمان يُنفَّذ
**مرة واحدة على الأكثر** في كل مرة، مع انتهاء التشغيلة دائمًا مكتملة أو مُتوقّفة، لا مضاعفة أبدًا.

المُختبِر مُصدَّر (`chaos/`) ومُوجَّه إلى SDKs أخرى في `benchmarks/`. النتيجة المقيسة:
**Bide `maxFired=1` (نجاح)؛ trpc-agent-go `maxFired=6`؛ langchaingo `maxFired=64` (كلاهما فشل).**
نقطة التحقّق/الاستئناف في trpc تعمل حقًّا (مُتحقَّق: استئناف تشغيلة مكتملة لا عملية له)؛ وتضاعُفها هو نافذة
LangGraph الموثّقة «يجب أن تكون العُقَد عديمة الأثر عند التكرار». وlangchaingo بلا معمورية إطلاقًا، فإعادة
المحاولات تعيد تشغيل كل شيء. وعلامة محاولة Bide تُغلِق النافذة تمامًا.

`WithMaxTurns(n)` يحدّ أدوار النموذج لكل تشغيلة فلا يستطيع نموذج يظلّ ينادي الأدوات أن يدور إلى الأبد:
بلوغه يُرجِع `ErrMaxTurns` (وهو `errors.Is` لـ `ErrBudget`). ويحدّ `WithTokenBudget(n)` الرموزَ التي
قد تستخدمها تشغيلة، شاملةً المدخلات المُخزَّنة مؤقتًا: فمتى استخدمت التشغيلة `n`، لا تُجري أي نداء نموذج آخر
وتُرجِع `ErrBudgetExceeded`. ويُسجَّل استهلاك كل نداء مع دوره، فيُعاد بناء الحدَّين كليهما من السجل ويصمدان عبر
الانهيار والاستئناف.

## العنصر البشري في الحلقة (Human-in-the-loop)

ثلاث نكهات. **موافقة/رفض**: أداة موسومة بـ `RequiresApproval` تتوقف *قبل* التشغيل؛ وقرار الإنسان قيمة بوليانية:

```go
_, err := a.Run(ctx, runID, input)
var pend *agent.PendingApproval
if errors.As(err, &pend) {
	// ... get a human decision ...
	agent.Approve(ctx, store, runID, pend.ToolUseID, true)
	out, _ := a.Run(ctx, runID, input) // resumes past the pause
}
```

**مقاطعة/استئناف**: أداة تتوقف *عند نقطة اعتباطية* وتستأنف بقيمة *مُصنَّفة* (تعمّم البوليان). استدعِ
`agent.Interrupt[T]` داخل أداة آمنة عند إعادة المحاولة:

```go
tool := agent.Func("choose_plan", "pick a plan", agent.Safety{ReadOnly: true},
	func(ctx context.Context, in Options) (Plan, error) {
		pick, err := agent.Interrupt[Plan](ctx, "plan", in) // pauses the run; in is shown to the human
		if err != nil {
			return Plan{}, err // *Interrupted propagates out of Run
		}
		return pick, nil // on resume, pick is the human's typed answer
	})

_, err := a.Run(ctx, runID, input)
var intr *agent.Interrupted
if errors.As(err, &intr) {
	// ... show intr.Prompt, get a typed answer ...
	agent.Resume(ctx, store, runID, intr.Key, chosenPlan)
	out, _ := a.Run(ctx, runID, input) // resumes; Interrupt now returns chosenPlan
}
```

كلاهما مُعمَّر: القرار/القيمة خطوةٌ مُسجَّلة، فينجو من انهيار. ويجب أن تكون المقاطعة في أداة آمنة عند إعادة
المحاولة (`ReadOnly`/`Idempotent`): عند الاستئناف تُعاد الأداة حتى تُحلّ المقاطعة، فكل ما قبل نداء
`Interrupt` يجب أن يكون آمنًا للتكرار.

**موافقة m-من-n**: حين لا يكفي توقيع واحد، اشترط k قرارات موقَّعة من مجموعة مُسمّاة من n مُوافِقين. يوقّع كل
مُوافِق النداءَ بعينه (الأداة ووسائطها)؛ وتمضي البوّابة عند k موافقات، وترفض متى صار بلوغ k مستحيلًا، وإلا
تتوقف مع الحصيلة الجارية. ويُتجاهَل القرار المُزوَّر أو الخاطئ دون أن يُقفَل مُوافِقه خارجًا:

```go
refund := agent.Func("refund", "refund the order",
	agent.Safety{Approval: &agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}},
	doRefund)
a := agent.New(model, store, refund).WithApproverVerifiers(keysByApprover)

// each approver, out of band, signs the paused call they were shown:
sig, _ := signer.Sign(agent.ApprovalDecisionBytes(pend.Subject(), "finance", true))
agent.ApproveAs(ctx, store, pend.RunID, pend.ToolUseID, "finance", true, sig)
```

ثم يُثبِت `audit.ApprovalEvidence` و`audit.VerifyApprovals` (أو `bide-audit verify-approvals`) دون اتصال أن
k مُوافِقين مُسمَّين وقّعوا على هذا النداء بعينه *قبل* تنفيذه، تحت السياسة المتوقَّعة، من أدلّة لا يمكن أن تُسقِط
قرارًا دون أن يُلاحَظ. انظر [دليل الموافقة](../../docs/guides/approval.md)؛ قابل للتشغيل عبر عمليات منفصلة في
`examples/approval`.

## الأخطاء

تُصنَّف الإخفاقات بأخطاء دالّة (sentinel errors) تُطابَق بـ `errors.Is`، وهو اصطلاح المكتبة القياسية، بلا
إطار أخطاء مخصّص. مستويان: **صنف (category)** (الفئة العامة) و**شرط (condition)** (سبب محدّد) يلفّ صنفه،
فتعمل المطابقة عند المستوى الذي تحتاجه:

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

الأصناف: `ErrConfig`، `ErrModel`، `ErrTool`، `ErrStorage`، `ErrProtocol`، `ErrBudget`.
الشروط (يلفّ كلٌّ منها صنفًا): `ErrUnknownTool`، `ErrToolArgs` (يلفّان `ErrTool`)،
`ErrToolReinvoked`، `ErrInvalidApproval`، `ErrAlreadyDecided` (تلفّ `ErrConfig`)،
`ErrNoRecordedOutput`، `ErrIncompleteResponse` (يلفّان `ErrModel`)، `ErrTruncatedToolArgs` (يلفّ
`ErrProtocol`)، `ErrBudgetExceeded`، `ErrMaxTurns` (يلفّان `ErrBudget`). وتُرجِع مُحوّلات المورّدين أيضًا
`*RateLimited` (HTTP 429، مع تلميح `RetryAfter`) و`*APIError` (أي استجابة غير 2xx أخرى، مع `StatusCode`)،
وكلاهما يلفّ `ErrModel`. كل خطأ تُرجِعه العُدّة (بما فيه من النموذج وMCP والمخزن ومُحوّلات الحوكمة) يحمل صنفًا،
فـ `errors.Is` موثوق عبر السطح كله.

**إشارات التحكّم في التدفّق** أغنى من صنف، فتبقى أنواعًا ملموسة تُطابَق بـ `errors.As`: `*PendingApproval`
(الموافقة مطلوبة)، `*Interrupted` (بانتظار مُدخَل بشري)، `*Sleeping` (مؤقّت مُعمَّر مُعلَّق)، `*Awaiting`
(بانتظار إشارة خارجية)، `*ResumeHalt` (غير آمن للاستئناف)، `*SagaAborted` (تراجَع)، و`*HaltTooYoung` (من
`ResolveHalt`، حين لم تنقضِ مدّة `WithMinHaltAge` بعد). التشغيلة المتوقّفة أو
المُتوقِّفة ليست صنف «إخفاق»؛ افحص البنية للحصول على `RunID` / `ToolUseID` / تفاصيل التعويض. ويظهر الإلغاء
كـ `context.Canceled` / `context.DeadlineExceeded` المعتادَين.

## Middleware والملاحظة (Observability)

سلسلتان مستقلّتان من نوع `func(Handler) Handler` عند الحدَّين المهمَّين: نداء النموذج (`Use`) وكل نداء أداة
(`UseTool`). أول مُضاف = الأخرج. وكلتاهما *مُغيِّرتان وقاطعتان للدائرة (short-circuiting)*: أعِد كتابة ما
يدخل، وحوّل ما يخرج، أو ارجِع دون استدعاء `next`.

```go
var cost middleware.CostMeter
a := agent.New(model, store, tools...).
	WithTokenBudget(100_000). // per run, rebuilt from the journal on resume
	Use(
		middleware.Retry(3, middleware.WithBackoff(200*time.Millisecond, 10*time.Second)),
		middleware.Cost(&cost, middleware.Rates{InputPer1M: 3, OutputPer1M: 15}),
	).
	UseTool(middleware.ToolLog(log.Printf), middleware.ToolCache(), middleware.ToolRetry(3))

// opt-in OTel gen_ai.* spans; the core has no OTel dependency:
a.Use(trace.Model(tracer, trace.WithSystem("openai"), trace.WithModel("gpt-4o-mini")))
a.UseTool(trace.Tool(tracer)) // execute_tool span per call; nests across the sub-agent boundary
// ... after the run: cost.Total() (USD), cost.Usage()
```

يقوم `Retry` بتراجع أُسّي مع اهتزاز (jitter) ويحترم `Retry-After` عند 429 من المورّد (يُرجِع المُحوّل نوعًا
مُصنَّفًا `*agent.RateLimited`)؛ ويُراكِم `Cost` الدولارات من استهلاك الرموز (شاملًا القراءة/الكتابة من
التخزين المؤقت) في `CostMeter` تقرؤه بعد التشغيلة.

لأن `trace.Tool` يعمل داخل الحلقة، يجلس نطاقه (span) في السياق المُسلَّم إلى الأداة، فحين تكون الأداة نفسها
وكيلًا فرعيًّا، تتداخل تشغيلة الوكيل الفرعي ونطاقاته كأبناء. ويعبر التتبّع حدّ الوكيل الفرعي تلقائيًّا (وهي
فجوة في ADK / AgenticGoKit / trpc-agent-go).

يعمل middleware الأداة *داخل* الخطوة المُعمَّرة، فقطع الدائرة (إصابة `ToolCache`) أو رفض السياسة يُسجَّل كأي
نتيجة أداة؛ ويعيد الاستئناف تشغيله ولا يعيد تشغيل الـ middleware ولا الأداة أبدًا. لا يعمل `ToolRetry`
و`ToolCache` إلا على الأدوات التي يسمح `Safety` الخاص بها بذلك (الآمنة لإعادة المحاولة، و`ReadOnly`، على
الترتيب)، ويُشغّل الوكيل الأداة غير الآمنة لإعادة المحاولة مرة واحدة على الأكثر لكل نداء أيًّا كان ما يفعله
الـ middleware. اكتب خاصّتك بتوقيع
`agent.ToolMiddleware`:

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

## الوحدات (Modules)

Bide مستودع متعدّد الوحدات: **نواة** خفيفة التبعيات (`github.com/bide-ai/bide`، الحلقة، schema،
middleware، مُحوّلات النموذج، باني تدفّق `plan`، `audit`، govern؛ تبعياتها مجرّد gsm + `x/sync`) مع وحدة
واحدة لكل مُحوّل ثقيل (`mcp`، `trace`، `store/sqlite`، `store/postgres`، `govern/redislog`،
`govern/sqlitelog`، `govern/postgreslog`، `codec/gcf`). استورد مُحوّلًا فتسحب شجرة تبعياته؛ واستورد النواة فقط فلا
تسحبها. سطح الوحدات الخارجية لمُستهلِك النواة-فقط هو 2، لا 54. انظر
[docs/reference/module-structure.md](../../docs/reference/module-structure.md).

## البنية (Architecture)

سُداسية بالبناء (Hexagonal): تعرّف النواة المنافذ (`Model`، `Durable`، `Tool`، `Middleware`)؛ وتُوصَل
المُحوّلات عند الحواف. التبعيات تشير إلى الداخل؛ والنواة لا تستورد أي مُحوّل ولا أي بنية تحتية، محروسة بـ
`architecture_test.go`.

```
agent (root)     durable loop · Message/Part · Tool/Safety · Durable · middleware types · RenderMermaid
plan             optional typed flow builder + declarative config; lowers to the loop (Topology · Conform)
model/anthropic  native Claude (thinking + signatures)
model/openai     any OpenAI-compatible endpoint
model/gemini     native Gemini (generativelanguage / Vertex via WithBaseURL)
schema           reflect Go types → inline JSON Schema + OpenAIStrict
middleware       Retry, RateLimit, Cost, Hedge
trace            opt-in OTel gen_ai.* spans
store/sqlite     on-disk durable resume (single binary, no cluster)
store/postgres   HA durable resume (any node resumes any run)
govern           Tier-2: federated governed state + quorum for agents that must agree (gsm-backed)
```

## الحوكمة الاتحادية: وكلاء يتّفقون، بإثبات (Tier-2)

النواة المُعمَّرة تُبقي عمل وكيل *واحد* آمنًا عند الانهيار. طبقة `govern` تعالج الحالة الصعبة الأخرى: **عدّة
وكلاء مُشغَّلين مستقلًّا عليهم أن يتّفقوا**، عبر حدود العمليات أو الفِرق أو المنظّمات، بلا منسّق مركزي وبلا
كاتب وحيد. تمنح صورتين قابلتين للتحقق من الاتّفاق، وفي كلتيهما الغاية *تحقّق، لا تثق*: يفحص طرفٌ النتيجة من
آثار عامّة دون الوثوق بوكيل أي طرف آخر.

**الاتّفاق على حالة مشتركة (تقارب).** صِف الحالة المشتركة كسجلّ (variables + invariants + events)؛ ويُثبِت
gsm *وقت البناء* أن كل تشابك لأفعال الوكلاء يبلغ الحالة الصحيحة نفسها، أو يرفض البناء ويُظهِر لك مثالًا
مضادًّا. زمن التشغيل بحثُ جداول بتعقيد O(1)؛ والحالة مصدرها الأحداث (event-sourced) وقابلة للاستعادة عند
الانهيار. يتوسّع من سجلّ مشترك وحيد صعودًا عبر **اتحادات (federations)** (قيود عبر الحدود: أشجار، ورسوم
DAG متعدّدة المصادر بمُحلِّلات، وشِباك (meshes) دورية رتيبة)، ويؤلَّف عبر `Embed`، ويستطيع حتى **تركيب**
التعويض لك (أعلِن القواعد، احصل على حاكم متقارب، أو برهان بعدم وجوده). وتُوصَل الوكلاء عبر
`FederatedEventTool`، فيصبح نداء أداة LLM حدثًا محكومًا.

**الاتّفاق على قرار (نِصاب / quorum).** k-من-n من الناخبين المُسمَّين (كلٌّ نموذج، أو مورّد، أو أصيل)
يُدلي بقرار مُطبَّع؛ وكل صوت خطوة مُسجَّلة «مرة واحدة على الأكثر» تُسجّل من صوّت وكيف، وبوّابة k-من-n حُكمُ
gsm فوق عدّ الأصوات، فـ «اتّفق k» مفحوص آليًّا فوق كل تعداد ممكن. يعيد `bide-audit verify-quorum` فحص
التعداد وكل صوت من آثار عامّة، مُعيدًا إنتاج قاعدة الأغلبية دون الوثوق بالمُنتِج. والادّعاء دقيق: النِّصاب
يُثبت *أن k ناخبين اتّفقوا* ويخفّض خطر النموذج الواحد؛ ولا يشهد بأن القرار صحيح (الأخطاء المترابطة ليست
استقلالًا)، وفقط القرارات المُطبَّعة يمكن أن تُنَصَّب، لا النثر الحرّ.

```go
gov, _ := govern.NewPersistent(ctx, machine, log, "order-42", machine.NewState())
tool := govern.EventTool(gov, "pay", "mark the order paid", "pay", agent.Safety{})
// hand `tool` to the agent: concurrent agents sharing `gov` converge, durably.
```

> الدليل الكامل، وسُلَّم القدرات، والعروض القابلة للتشغيل (`examples/mesh`، `examples/compose`،
> `examples/quorum`) في **[docs/guides/governance.md](../../docs/guides/governance.md)**.

## الأدلّة (Guides)

جديد هنا؟ ابدأ بـ **[البدء](../../docs/getting-started.md)**، واستخدم **[فهرس الوثائق](../../docs/README.md)**
للخريطة الكاملة، وانظر **[المفاهيم](../../docs/CONCEPTS.md)** للمفردات (journal، at-most-once، lease، Waker،
gsm، ProofBundle). ويُذكَر ضمان المعمورية الدقيق في **[الضمان](../../docs/GUARANTEE.md)** وحدوده في
**[القيود المعروفة](../../docs/KNOWN-LIMITATIONS.md)**.

**التأليف**

- **[التدفّقات](../../docs/guides/flows.md)**: باني تدفّق `plan` المُصنَّف. ألِّف طوبولوجيا
  (`Step`/`Tool`/`Model`/`Switch`/`Join`/`LoopBack`) تُنزَّل إلى السجل نفسه، ثم أثبِت أن تشغيلة اتّبعتها
  (`Conform`). قابل للتشغيل: `examples/plan`.
- **[الخطوات المُعمَّرة](../../docs/guides/durable-steps.md)**: ألِّف عملك المُعمَّر الخاص: `Step`، وتجميع
  `Parallel`/`Task`، والملاحم (`RunSaga`)، والمؤقّتات المُعمَّرة (`Sleep`/`WaitUntil`). قابل للتشغيل:
  `examples/parallel`.
- **[الموثوقية](../../docs/guides/reliability.md)**: مهلات لكل محاولة، وإعادة محاولة مُصنَّفة، ونداءات نموذج
  مُتحوَّطة، وتحديد المعدّل، وتتبّع التكلفة، وكيف تتألّف. قابل للتشغيل: `examples/hedge`.
- **[الإشارات والوكلاء المُحيطيّون](../../docs/guides/signals.md)**: أحداث خارجية إلى داخل تشغيلة: المؤقّتات
  المُعمَّرة و`Waker`، والعنصر البشري في الحلقة (`Interrupt`/`Resume`)، والإشارات المُعمَّرة (دخول «مرة واحدة
  على الأقل»، وتطبيق «مرة واحدة بالضبط»). قابل للتشغيل: `examples/signals`، `examples/interrupt`.
- **[النماذج](../../docs/guides/models.md)**: مُحوّلات Anthropic والمتوافق مع OpenAI وGemini: `WithBaseURL`،
  والمعاينة، والتخزين المؤقت للموجّهات، والأخطاء المُصنَّفة، وإدخال الصور المتعدّد الوسائط.
- **[MCP](../../docs/guides/mcp.md)**: وصِّل خادم MCP كمصدر أدوات وقت التشغيل، مع استئناف آمن للأثر الجانبي؛
  ويمكن لتعليقات أدوات خادم موثوق أن تَسِم الأدوات بأنها آمنة لإعادة التشغيل.
- **[الملاحظة](../../docs/guides/observability.md)**: نطاقات OTel gen_ai في سطر واحد (`trace.Instrument`):
  تصنيف النطاقات، وتداخل الوكيل الفرعي، وتحويل الرموز إلى تكلفة، وافتراض خصوصية التقاط المحتوى. قابل للتشغيل:
  `examples/observability`.
- **[المراسلة](../../docs/guides/messaging.md)**: قُد وكيلًا من webhook وارد (Slack، Telegram، SMS،
  Discord) بأمان عند إعادة التسليم: webhook مُعاد المحاولة يُعيد التشغيل بدل التنفيذ المزدوج. قابل للتشغيل:
  `examples/webhook`.
- **[التصحيح والاستعادة](../../docs/guides/debugging.md)**: إعادة التشغيل الحتمية (`Replay`)، وإعادة بناء
  الأحداث (`ReplayEvents`)، ورسوم Mermaid للتشغيلات، واستعادة الانهيار (`Recover`) التي تعيد قيادة
  التشغيلات المُقاطَعة.

**المساءلة والحوكمة**

- **[التدقيق](../../docs/guides/audit.md)**: تشغيلات حاملة للبرهان. تشحن تشغيلة `RunCertificate` واحدة قابلة
  للنقل، تُفحَص دون اتصال بـ `bide-audit verify-run`. قابل للتشغيل: `examples/proof-carrying-run`.
- **[التفويض](../../docs/guides/delegation.md)**: مِنَح قدرات موقَّعة لا يستطيع الوكيل الفرعي إلا تضييقها
  (`Grant`/`SignGrant`)، مُتحقَّق منها دون اتصال (`VerifyDelegationChain`)، مع سُلطة مكتسَبة من أثر نظيف. قابل
  للتشغيل: `examples/delegation`، `examples/authority`.
- **[نموذج الأمان](../../docs/guides/security-model.md)**: النطاق الدقيق للضمانات التشفيرية (السلامة،
  والأصالة، وإظهار العبث، وعدم الإنكار، والإفصاح الانتقائي) وما هو خارج النطاق (السرّية). اقرأه قبل الاعتماد
  على الأثر.
- **[الحوكمة](../../docs/guides/governance.md)**: ركيزة الحالة المحكومة من Tier-2 (gsm). صِف الحالة المشتركة
  كسجلّ، ويُثبِت `Build()` أن كل تشابك يتقارب أو يُعيد مثالًا مضادًّا. قابل للتشغيل: `examples/mesh`،
  `examples/compose`.
- **[الموافقة](../../docs/guides/approval.md)**: توقيع بشري مُعمَّر قبل تشغيل أداة، من 1-من-1 إلى m-من-n
  الموقَّعة (`ApprovalPolicy`، `ApproveAs`)، مع برهان دون اتصال على أن k مُوافِقين مُسمَّين وافقوا قبل الفعل
  (`audit.ApprovalEvidence`، `audit.VerifyApprovals`). قابل للتشغيل: `examples/approval`.
- **[النِّصاب](../../docs/guides/quorum.md)**: اتّفاق نماذج محكوم k-من-n (`govern.Quorum`)، مع تثبيت التعداد
  في السجل وقابليته لإعادة الفحص دون اتصال (`bide-audit verify-quorum`). قابل للتشغيل: `examples/quorum`.

**المرجع والبنية الداخلية**

- **[نقاط التوسعة](../../docs/reference/extension-points.md)**: المنافذ والمُحوّلات (`Model`، `Durable`،
  `Tool`، `Compensator`، `Retriever`، `Anchor`، `EventStore`)، مع جولة «نفّذ مخزنك الخاص».
- **[كيف يُتحقَّق من bide](../../docs/testing/verification.md)**: لا إصلاح بلا اختبار فاشل، وفحوص الطفرات،
  ومسوح الانهيار والإلغاء، والتشابكات المفروضة، وأجنحة المطابقة، وما يفرضه الـ CI.
- **[الاختبار والأدلّة](../../docs/testing/testing.md)**: ما الذي يُختبَر وكيف، واختبار أداء حقن الانهيارات
  chaos، وأوراكل تفاضلية، ومطابقة RFC 6962، وحدّ المُبرهَن-مقابل-الإحصائي في حزمة `eval`.
- **[ضغط السجل](../../docs/design/compaction.md)** (مذكّرة تصميم): ضغط سجلّ غير محدود دون كسر براهين الشمول
  والاتّساق للعمود الفقري للتدقيق.

## التواصل

أسئلة، أو ملاحظات، أو اهتمام باستخدام bide: **dayna@blackwell-systems.com**. تُرسَل مشكلات الأمان عبر
[SECURITY.md](../../SECURITY.md) (إبلاغ خاص).

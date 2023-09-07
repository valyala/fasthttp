### TL;DR

We use a simplified version of [Golang Security Policy](https://go.dev/security).
For example, for now we skip CVE assignment.

### Reporting a Security Bug

Please report to us any issues you find. This document explains how to do that and what to expect in return.

All security bugs in our releases should be reported by email to erik@dubbelboer.com
Your email will be acknowledged within 24 hours, and you'll receive a more detailed response
to your email within 72 hours indicating the next steps in handling your report.
Please use a descriptive subject line for your report email.

### Flagging Existing Issues as Security-related

If you believe that an existing issue is security-related, we ask that you send an email to erik@dubbelboer.com
The email should include the issue ID and a short description of why it should be handled according to this security policy.

### Disclosure Process

Our project uses the following disclosure process:

- Once the security report is received it is assigned a primary handler. This person coordinates the fix and release process.
- The issue is confirmed and a list of affected software is determined.
- Code is audited to find any potential similar problems.
- Fixes are prepared for the two most recent major releases and the head/master revision. These fixes are not yet committed to the public repository.
- To notify users, a new issue without security details is submitted to our GitHub repository.
- Three working days following this notification, the fixes are applied to the public repository and a new release is issued.
- On the date that the fixes are applied, announcement is published in the issue.

This process can take some time, especially when coordination is required with maintainers of other projects.
Every effort will be made to handle the bug in as timely a manner as possible, however it's important that we follow
the process described above to ensure that disclosures are handled consistently.

### Receiving Security Updates
The best way to receive security announcements is to subscribe ("Watch") to our repository.
Any GitHub issues pertaining to a security issue will be prefixed with [security].

### Comments on This Policy
If you have any suggestions to improve this policy, please send an email to erik@dubbelboer.com for discussion.
